package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wykserdex/wedra/internal/core"
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
)

const (
	// defaultProtocolVersion — ревизия, которую сервер выбирает, когда клиент
	// не назвал свою. 2025-06-18, а не 2024-11-05: с неё annotations
	// (readOnlyHint/destructiveHint) входят в контракт, и хост по ним решает,
	// показывать ли человеку подтверждение перед запуском инструмента.
	//
	// Ревизии 2026-07-28 (текущей на 2026-10-01) здесь нет сознательно: она
	// убрала хендшейк initialize в пользу server/discover и _meta на каждом
	// запросе. Объявить поддержку ревизии, обязательный путь которой не
	// реализован, — хуже, чем честно назвать ту, что реализована.
	defaultProtocolVersion = "2025-06-18"
	maxWaitSeconds         = 300
	maxRetainedRuns        = 128

	// P2 F-03: потолки чтения журнала в инструментах (та же цена, что и в
	// HTTP API: журнал рана не ограничен). Статус/pending_gate смотрят на
	// последние события, а get_run отдаёт окно от since с курсором next.
	maxRunJournalEvents = 2000
	maxRunJournalBytes  = 4 << 20
)

// supportedProtocolVersions — ревизии, которые сервер готов вести.
//
// Порядок важен: на версию, которой нет в списке, сервер отвечает ПЕРВОЙ
// поддерживаемой (negotiateProtocol), то есть самой новой из своих. Раньше в
// списке была одна старая ревизия, и клиент, просивший 2025-06-18 или
// 2026-07-28, молча получал 2024-11-05 — без признака того, что его понизили.
var supportedProtocolVersions = []string{defaultProtocolVersion, "2024-11-05"}

// Server — MCP-сервер: JSON-RPC stdio + 8 инструментов поверх core/pipeline/execution.
// Решения гейтов через MCP невозможны никогда; get_run в waiting_human
// просит пользователя одобрить в окне wedra.
type Server struct {
	pluginsDirs []string
	workDir     string
	runsDir     string
	engine      *core.Engine
	multi       *multiEngine

	human HumanChannel
	// trust — политика доверия сервера. exec_plugin без AgentCanExec
	// отказывает, а запуск идёт через ExecWithEnvCtx, чтобы enforceTrust
	// действительно применился.
	trust plugin.TrustPolicy
	// execSem — потолок одновременных запусков плагина агентом.
	// Инициализируется в NewServer; nil означает «слоты не настроены», и
	// exec_plugin тогда отказывает, а не выполняется без предела.
	execSem chan struct{}
	// allowUngatedRuns — обход требования одобрения (см. Options).
	allowUngatedRuns bool

	mu           sync.Mutex
	running      bool
	currentRunID string
	runs         map[string]*runState
	cancels      map[string]context.CancelFunc
	// inflight — вызовы инструментов в работе, по id запроса: адресат
	// нотификации notifications/cancelled. По JSON-RPC отмена приходит на
	// конкретный запрос, а не на ран, поэтому связь «запрос → ран» хранит
	// сервер, а не клиент.
	inflight map[string]*inflightCall
	// transport — сессия stdio. Нужен не только для ответов: с элиситацией
	// сервер впервые сам отправляет запросы клиенту, а не только отвечает.
	transport   *Transport
	pending     map[string]chan *clientResponse
	outboundSeq int64
	// clientElicitation — клиент объявил поддержку elicitation в initialize.
	clientElicitation bool
	// gateElicitation — оператор разрешил вести гейты через клиента
	// (--gate-elicitation). Отдельно от поддержки клиента: видеть поддержку и
	// доверять ей — разные решения.
	gateElicitation bool
	// runRequests — id запроса → ран, живущий ДОЛЬШЕ своего запроса. Ран не
	// заканчивается вместе с ответом run_pipeline: он уходит в waiting_human
	// или продолжается в фоне, и связь нужна, пока он жив. Иначе отмена,
	// пришедшая после ответа (человек нажал Esc в консоли клиента, хост
	// оборвал сессию), не находила бы ран и оставляла занятым единственный
	// слот — тот самый E_RUN_BUSY.
	runRequests map[string]string
}

// inflightCall — вызов инструмента, который может быть отменён нотификацией.
//
// Поля заполняются по мере того, как инструмент доходит до своей длинной
// части: у run_pipeline это старт рана, у exec_plugin — запуск процесса.
// Нотификация может прийти раньше (клиент не обязан ждать) — тогда
// cancelled=true, и bind* отменяет работу сразу после её появления.
type inflightCall struct {
	// key — id запроса, которым вызов пришёл: по нему ран связывается с
	// запросом, даже когда сам вызов уже завершился.
	key    string
	runID  string
	writer *Transport
	// progressToken — _meta.progressToken из запроса: без него спека
	// запрещает слать notifications/progress (клиент не сможет сопоставить).
	progressToken json.RawMessage
	cancelExec    context.CancelFunc
	cancelled     bool
}

type runState struct {
	id      string
	dir     string
	done    chan struct{}
	status  string
	errMsg  string
	code    string
	okItems int
	aborted int
	// cancelRequested — отмена пришла до того, как ран зарегистрировал свой
	// cancel. Без этого флага нотификация, обогнавшая старт рана, терялась
	// бы: cancel() в s.cancels появляется на горутине рана, а не в момент
	// создания состояния.
	cancelRequested bool
}

// Options — флаги wedra mcp --plugins ... --workdir ...
type Options struct {
	PluginsDirs []string
	WorkDir     string
	RunsDir     string
	// Agent-track human channel for gates and cancellation.
	// nil — гейты MCP-ранов одобрить некому: run_pipeline с human_gate
	// отклоняется с E_NO_HUMAN_CHANNEL, а не висит в waiting_human вечно.
	Human HumanChannel
	// Trust — политика доверия, действующая на весь MCP-сервер.
	//
	// Нужна для exec_plugin: раньше инструмент был доступен всегда, потому
	// что AgentCanExec никто не проверял, а ExecWithEnv уходил с
	// context.Background(), то есть с нулевой политикой. Политика приходит
	// из флагов командной строки (cli.RunMCP).
	Trust plugin.TrustPolicy
	// GateElicitation — вести гейты через elicitation/create, если клиент
	// объявил эту возможность (ревизия 2025-06-18). По умолчанию выключено:
	// wedra не может проверить, что за клиентом сидит человек, а назвать
	// решение автономного хоста решением человека — соврать в журнале.
	GateElicitation bool
	// AllowUngatedRuns — операторский обход требования одобрения: ран с
	// опасным шагом и без human_gate разрешается. Обход НЕ молчаливый: каждый
	// такой запуск пишется в <runs-dir>/gate-bypass.jsonl, а неудачная запись
	// означает отказ (неоплаченный запуск недопустим). Локальное доверенное
	// использование, а не режим по умолчанию.
	AllowUngatedRuns bool
}

// HumanChannel — куда MCP-сервер отдаёт гейты ранов. Реализация (cli/mcp.go)
// регистрирует ChannelUI во встроенном HTTP-сервере с сессией и открывает
// браузер человеку. Агенту URL с ключом не выдаётся никогда.
type HumanChannel interface {
	AttachRun(runID string, ui *gate.ChannelUI, cancel context.CancelFunc)
	DetachRun(runID string)
	// GateWaiting — ран дошёл до гейта (первый раз): показать человеку.
	GateWaiting(runID string)
	// PublicURL — адрес консоли БЕЗ ключа (для подсказки агенту).
	PublicURL(runID string) string
}

func NewServer(opts Options) (*Server, error) {
	workDir := opts.WorkDir
	if workDir == "" {
		cwd, _ := os.Getwd()
		workDir = cwd
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	var absPlugins []string
	for _, d := range opts.PluginsDirs {
		if strings.TrimSpace(d) == "" {
			return nil, fmt.Errorf("плагины: каталог не указан")
		}
		if !filepath.IsAbs(d) {
			d = filepath.Join(absWork, d)
		}
		a, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		absPlugins = append(absPlugins, a)
	}
	if len(absPlugins) == 0 {
		absPlugins = []string{filepath.Join(absWork, "plugins")}
	}
	runsDir := opts.RunsDir
	if runsDir == "" {
		runsDir = journal.DefaultRunsDirAt(absWork)
	} else if !filepath.IsAbs(runsDir) {
		runsDir = filepath.Join(absWork, runsDir)
	}
	if err := os.MkdirAll(runsDir, 0755); err != nil {
		return nil, err
	}
	eng := core.NewEngine()
	eng.PluginsDir = absPlugins[0]
	multi := &multiEngine{dirs: absPlugins, workDir: absWork}
	return &Server{
		pluginsDirs:      absPlugins,
		workDir:          absWork,
		runsDir:          runsDir,
		engine:           eng,
		multi:            multi,
		runs:             map[string]*runState{},
		cancels:          map[string]context.CancelFunc{},
		human:            opts.Human,
		trust:            opts.Trust,
		execSem:          make(chan struct{}, MaxConcurrentAgentExec),
		allowUngatedRuns: opts.AllowUngatedRuns,
		gateElicitation:  opts.GateElicitation,
		pending:          map[string]chan *clientResponse{},
	}, nil
}

// checkPluginRef — песочница: абсолютные пути и .. за пределами корней запрещены.
func (s *Server) callTool(call *inflightCall, name string, args map[string]interface{}) (string, bool, *RPCError) {
	if args == nil {
		args = map[string]interface{}{}
	}
	switch name {
	case "list_plugins":
		return s.toolListPlugins(args)
	case "describe_plugin":
		return s.toolDescribePlugin(args)
	case "validate_pipeline":
		return s.toolValidate(args)
	case "plan_pipeline":
		return s.toolPlan(args)
	case "run_pipeline":
		return s.toolRun(call, args)
	case "get_run":
		return s.toolGetRun(args)
	case "cancel_run":
		return s.toolCancel(args)
	case "exec_plugin":
		return s.toolExecPlugin(call, args)
	default:
		// Спека MCP называет этот случай протокольной ошибкой с кодом
		// -32602 («Unknown tool: X»). -32601 означает «нет такого JSON-RPC
		// метода» и сбивает клиента с толку: метод tools/call существует.
		return "", false, &RPCError{Code: -32602, Message: "unknown tool: " + name}
	}
}

func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func permissionsJSON(m *pipeline.Manifest) map[string]interface{} {
	network := make([]map[string]interface{}, 0, len(m.Permissions.Network))
	for _, np := range m.Permissions.Network {
		network = append(network, map[string]interface{}{
			"host": np.Host, "port": np.Port, "any_host": np.AnyHost, "note": np.Note,
		})
	}
	return map[string]interface{}{
		"network": network, "filesystem": m.Permissions.Filesystem, "secrets": m.Permissions.Secrets,
	}
}
