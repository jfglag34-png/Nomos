package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wykserdex/wedra/internal/buildinfo"
	"github.com/wykserdex/wedra/internal/common"
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
func (s *Server) checkPluginRef(ref string) error {
	if pipeline.IsBuiltin(ref) {
		return nil
	}
	if pipeline.IsBuiltinNamespace(ref) {
		return fmt.Errorf("E_PLUGIN_LOAD: неизвестный встроенный модуль: %s", ref)
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: путь %q выходит за корни плагинов", ref)
	}
	if s.multi != nil {
		return s.multi.checkPluginRef(ref)
	}
	isAbs := filepath.IsAbs(ref) || strings.HasPrefix(ref, "/")
	if isAbs {
		abs := ref
		if !filepath.IsAbs(abs) {
			return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: %q вне --plugins/--workdir", ref)
		}
		abs, _ = filepath.Abs(abs)
		for _, root := range append(append([]string{}, s.pluginsDirs...), s.workDir) {
			if pathWithinRoot(root, abs) {
				return nil
			}
		}
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: %q вне --plugins/--workdir", ref)
	}
	return nil
}

func (s *Server) checkPathInWorkdir(p string) error {
	if p == "" {
		return nil
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: path %q содержит NUL", p)
	}
	candidate := p
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(s.workDir, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil || !pathWithinRoot(s.workDir, abs) {
		return fmt.Errorf("E_PLUGIN_OUTSIDE_ROOT: path %q вне --workdir", p)
	}
	return nil
}

func resolvePathForContainment(path string) (string, error) {
	current, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current = filepath.Clean(current)
	for links := 0; links < 255; links++ {
		probe := current
		suffix := []string{}
		for {
			if target, linkErr := os.Readlink(probe); linkErr == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(probe), target)
				}
				current = filepath.Join(append([]string{target}, suffix...)...)
				break
			}
			if info, statErr := os.Lstat(probe); statErr == nil {
				if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
					return "", fmt.Errorf("reparse point: %s", probe)
				}
			} else if !os.IsNotExist(statErr) {
				return "", statErr
			}
			if resolved, evalErr := filepath.EvalSymlinks(probe); evalErr == nil {
				for _, part := range suffix {
					resolved = filepath.Join(resolved, part)
				}
				return filepath.Clean(resolved), nil
			}
			parent := filepath.Dir(probe)
			if parent == probe {
				return "", fmt.Errorf("path cannot be resolved: %s", current)
			}
			suffix = append([]string{filepath.Base(probe)}, suffix...)
			probe = parent
		}
	}
	return "", fmt.Errorf("too many links: %s", current)
}

func pathWithinRoot(root, path string) bool {
	rootAbs, err := resolvePathForContainment(root)
	if err != nil {
		return false
	}
	pathAbs, err := resolvePathForContainment(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func foreachInputKey(foreach, item string) string {
	if foreach == "" {
		return ""
	}
	if item == "" {
		return "item"
	}
	return item
}

func fileRefSourceDynamic(pf *pipeline.PipelineFile, st *pipeline.Step, source string) bool {
	if !strings.HasPrefix(source, "input.") {
		return true
	}
	key := strings.TrimPrefix(source, "input.")
	if key == "" || strings.Contains(key, ".") {
		return true
	}
	if pf.Pipeline.Foreach != "" && key == foreachInputKey(pf.Pipeline.Foreach, pf.Pipeline.ForeachItem) {
		return true
	}
	if st != nil && st.Foreach != "" && key == foreachInputKey(st.Foreach, st.ForeachItem) {
		return true
	}
	for i := range pf.Pipeline.Steps {
		step := &pf.Pipeline.Steps[i]
		if step.Foreach != "" && key == foreachInputKey(step.Foreach, step.ForeachItem) {
			return true
		}
	}
	return false
}

func (s *Server) Serve(t *Transport) error {
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	s.mu.Lock()
	s.transport = t
	s.mu.Unlock()
	for {
		frame, err := t.ReadFrame()
		if err != nil {
			wg.Wait()
			select {
			case writeErr := <-errCh:
				return writeErr
			default:
				return err
			}
		}
		if frame.Batch {
			// Порядок ответов батча обязан совпадать с порядком запросов
			// (JSON-RPC 2.0), поэтому батч идёт последовательно: параллелизм
			// внутри одного кадра не даёт ничего, кроме гонки за порядок.
			responses := make([]*Response, 0, len(frame.Items))
			for _, item := range frame.Items {
				if item.Err != nil {
					responses = append(responses, item.Err.response())
					continue
				}
				if item.Resp != nil {
					s.deliverClientResponse(item.Resp)
					continue
				}
				req := item.Request
				if isNotification(req) {
					s.handleNotification(req)
					continue
				}
				if len(req.ID) == 0 {
					continue
				}
				call := s.beginCall(t, req)
				res := s.handleCall(call, req)
				s.endCall(req)
				res.ID = req.ID
				responses = append(responses, res)
			}
			if len(responses) == 0 {
				continue // батч из одних нотификаций: отвечать нечем
			}
			if err := t.WriteBatch(responses); err != nil {
				return err
			}
			continue
		}
		item := frame.Items[0]
		if item.Err != nil {
			if err := t.Write(item.Err.response()); err != nil {
				return err
			}
			continue
		}
		if item.Resp != nil {
			// Ответ клиента на серверный запрос (elicitation): адресат ждёт его
			// на канале, ответа клиенту не требуется.
			s.deliverClientResponse(item.Resp)
			continue
		}
		req := item.Request
		if isNotification(req) {
			s.handleNotification(req)
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		call := s.beginCall(t, req)
		wg.Add(1)
		go func(req *Request, call *inflightCall) {
			defer wg.Done()
			defer s.endCall(req)
			res := s.handleCall(call, req)
			res.ID = req.ID
			if err := t.Write(res); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(req, call)
	}
}

// requestKey — ключ сопоставления запроса и нотификации об отмене. JSON-RPC
// разрешает id и числом, и строкой; клиент возвращает ровно то значение,
// которое отправил, поэтому ключом служат сырые байты, а не разобранное число.
func requestKey(id json.RawMessage) string {
	return strings.TrimSpace(string(id))
}

// beginCall — зарегистрировать вызов до его исполнения. Регистрация нужна
// именно в этот момент, а не при старте рана: нотификация об отмене может
// прийти, пока инструмент ещё валидирует пайплайн, и это не ошибка клиента.
func (s *Server) beginCall(t *Transport, req *Request) *inflightCall {
	key := requestKey(req.ID)
	call := &inflightCall{key: key, writer: t}
	if key == "" {
		return call
	}
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]*inflightCall{}
	}
	s.inflight[key] = call
	s.mu.Unlock()
	return call
}

func (s *Server) endCall(req *Request) {
	key := requestKey(req.ID)
	if key == "" {
		return
	}
	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
}

// bindRun — привязать к вызову запущенный им ран. Связь живёт, пока жив ран
// (s.runRequests), а не пока жив запрос: у MCP-клиента запрос заканчивается
// раньше рана.
func (s *Server) bindRun(call *inflightCall, runID string) {
	if call == nil || runID == "" {
		return
	}
	s.mu.Lock()
	call.runID = runID
	if call.key != "" {
		if s.runRequests == nil {
			s.runRequests = map[string]string{}
		}
		s.runRequests[call.key] = runID
	}
	late := call.cancelled
	if late {
		if st, ok := s.runs[runID]; ok {
			st.cancelRequested = true
		}
	}
	s.mu.Unlock()
	if late {
		// Отмена пришла раньше старта рана: без этой ветки нотификация
		// терялась бы, и ран доезжал бы до конца, а клиент — до таймаута.
		if rpcErr := s.cancelRun(runID); rpcErr != nil {
			s.logf("notifications/cancelled: ран %s отменить не удалось: %s", runID, rpcErr.Message)
		}
	}
}

// bindExec — привязать к вызову процесс exec_plugin.
func (s *Server) bindExec(call *inflightCall, cancel context.CancelFunc) {
	if call == nil || cancel == nil {
		return
	}
	s.mu.Lock()
	call.cancelExec = cancel
	late := call.cancelled
	s.mu.Unlock()
	if late {
		cancel()
	}
}

// handleNotification — нотификации жизненного цикла. Ответа не требуют
// (isNotification), но не все безопасны к игнорированию:
// notifications/cancelled означает «отмени работу запроса <id>». Раньше метод
// просто пропускался, и отменённый клиентом запрос продолжал держать
// единственный слот рана — агент до таймаута получал E_RUN_BUSY и не мог
// запустить ничего нового.
func (s *Server) handleNotification(req *Request) {
	if req.Method != "notifications/cancelled" {
		return
	}
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return
	}
	key := requestKey(p.RequestID)
	if key == "" {
		return
	}
	s.cancelInflight(key, p.Reason)
}

// cancelInflight — отмена по нотификации клиента.
//
// Ищутся два адресата, и порядок важен. Сначала живой вызов: его ран, если он
// есть, и запущенный процесс exec_plugin. Потом — ран, который пережил свой
// запрос (waiting_human или фоновый): буква спеки советует игнорировать
// нотификацию к завершённому запросу, но у нас «завершённый запрос» и
// «законченный ран» — разные вещи, и именно этот случай оставлял слот занятым
// до таймаута.
func (s *Server) cancelInflight(key, reason string) {
	s.mu.Lock()
	call := s.inflight[key]
	var runID string
	var cancelExec context.CancelFunc
	if call != nil {
		call.cancelled = true
		runID = call.runID
		cancelExec = call.cancelExec
	}
	if runID == "" {
		runID = s.runRequests[key]
	}
	if call == nil && runID == "" {
		s.mu.Unlock()
		s.logf("notifications/cancelled: запрос %s завершён и рана за ним нет — отменять нечего", key)
		return
	}
	s.mu.Unlock()
	if cancelExec != nil {
		cancelExec()
	}
	if runID == "" {
		// Инструмент ещё не дошёл до старта рана: отменять нечего, а признак
		// на вызове остался — bindRun отменит ран сразу после его появления.
		s.logf("notifications/cancelled: запрос %s отменён до старта рана (%s)", key, reason)
		return
	}
	if rpcErr := s.cancelRun(runID); rpcErr != nil {
		s.logf("notifications/cancelled: ран %s отменить не удалось: %s", runID, rpcErr.Message)
		return
	}
	s.logf("notifications/cancelled: ран %s отменён по запросу клиента (%s)", runID, reason)
}

// cancelRun — общая точка отмены: cancel_run и notifications/cancelled приходят
// сюда, чтобы «отменено» означало одно и то же независимо от того, кто просил.
func (s *Server) cancelRun(runID string) *RPCError {
	s.mu.Lock()
	st, known := s.runs[runID]
	cancel, hasCancel := s.cancels[runID]
	if !known {
		s.mu.Unlock()
		return rpcErr("", "ран не найден: "+runID)
	}
	if cancel == nil || !hasCancel {
		select {
		case <-st.done:
			s.mu.Unlock()
			return &RPCError{Code: -32000, Message: "ран уже завершён", Data: map[string]string{"code": "E_RUN_DONE"}}
		default:
			// cancel() регистрируется на горутине рана, то есть может ещё не
			// существовать. Запоминаем намерение: ран отменит себя сам, как
			// только зарегистрирует cancel.
			st.cancelRequested = true
			s.mu.Unlock()
			return nil
		}
	}
	s.mu.Unlock()
	cancel()
	return nil
}

func negotiateProtocol(params json.RawMessage) (string, error) {
	if len(params) == 0 || string(params) == "null" {
		return defaultProtocolVersion, nil
	}
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", err
	}
	if p.ProtocolVersion == "" {
		return defaultProtocolVersion, nil
	}
	for _, version := range supportedProtocolVersions {
		if p.ProtocolVersion == version {
			return version, nil
		}
	}
	return supportedProtocolVersions[0], nil
}

// handle — точка входа для тестов и внутренних вызовов: без привязки к id
// запроса, поэтому такой вызов нельзя отменить нотификацией.
func (s *Server) handle(req *Request) *Response { return s.handleCall(nil, req) }

func (s *Server) handleCall(call *inflightCall, req *Request) *Response {
	switch req.Method {
	case "initialize":
		version, err := negotiateProtocol(req.Params)
		if err != nil {
			return &Response{Error: &RPCError{Code: -32602, Message: "invalid initialize params: " + err.Error()}}
		}
		s.setClientCapabilities(req.Params)
		return &Response{Result: map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
			"serverInfo":      map[string]interface{}{"name": "wedra", "version": buildinfo.Resolve()},
		}}
	case "ping":
		return &Response{Result: map[string]interface{}{}}
	case "tools/list":
		return &Response{Result: map[string]interface{}{"tools": toolDefs()}}
	case "tools/call":
		var p struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
			Meta      struct {
				ProgressToken json.RawMessage `json:"progressToken"`
			} `json:"_meta"`
		}
		if req.Params != nil {
			_ = json.Unmarshal(req.Params, &p)
		}
		if call != nil && len(p.Meta.ProgressToken) > 0 {
			call.progressToken = p.Meta.ProgressToken
		}
		result, isErr, rpcErr := s.callTool(call, p.Name, p.Arguments)
		if rpcErr != nil {
			return &Response{Error: rpcErr}
		}
		out := map[string]interface{}{
			"content": []map[string]interface{}{{"type": "text", "text": result}},
			"isError": isErr,
		}
		// structuredContent (2025-06-18): тот же ответ объектом, а не только
		// строкой. Клиент получает типизированные данные без разбора текста, а
		// текстовый блок остаётся для совместимости (спека этого и требует).
		if structured := structuredContentOf(result, isErr); structured != nil {
			out["structuredContent"] = structured
		}
		return &Response{Result: out}
	default:
		return &Response{Error: &RPCError{Code: -32601, Message: "unknown method: " + req.Method}}
	}
}

// issuesForJSON — замечания всегда массив.
//
// nil-срез Go сериализует как `null`, а outputSchema обещает `array`:
// structuredContent с null нарушал бы контракт, который сервер же и объявил.
// Пустой список и «нет замечаний» — одно и то же, и `[]` это показывает.
func issuesForJSON(issues []pipeline.Issue) []pipeline.Issue {
	if issues == nil {
		return []pipeline.Issue{}
	}
	return issues
}

// structuredContentOf — объектный вид результата инструмента.
//
// nil там, где объекта нет или где это ошибка исполнения: спека требует, чтобы
// structuredContent соответствовал объявленной outputSchema, а isError-результат
// описывается другой схемой (и её у инструмента нет). Молча отдать туда
// строку сообщения об ошибке значило бы нарушить контракт.
func structuredContentOf(result string, isErr bool) map[string]interface{} {
	if isErr {
		return nil
	}
	trimmed := strings.TrimSpace(result)
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return nil
	}
	return obj
}

// rawMessage — вставка уже готового JSON (progressToken приходит от клиента).
type rawMessage json.RawMessage

func (r rawMessage) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return []byte(r), nil
}

// notifyProgress — notifications/progress (2024-11-05 и новее).
//
// Спека разрешает серверу слать прогресс, только если клиент дал
// progressToken в _meta запроса: без токена клиент не сопоставит нотификацию с
// вызовом. Нет токена — молчим, а не выдумываем свой.
//
// Зачем это вообще: run_pipeline умеет ждать до 300 секунд, и клиенты с
// таймаутом на tool-call обрывали вызов раньше, чем ран доходил до гейта.
// Прогресс говорит «работа идёт» и у большинства клиентов сбрасывает таймер.
func (s *Server) notifyProgress(call *inflightCall, progress float64, total float64, message string) {
	if call == nil || call.writer == nil || len(call.progressToken) == 0 {
		return
	}
	params := map[string]interface{}{
		"progressToken": rawMessage(call.progressToken),
		"progress":      progress,
	}
	if total > 0 {
		params["total"] = total
	}
	if message != "" {
		params["message"] = message
	}
	if err := call.writer.WriteNotification("notifications/progress", params); err != nil {
		// Прогресс — не часть контракта вызова: его потеря не должна ронять
		// инструмент, но и молчать о ней нельзя.
		s.logf("notifications/progress не отправлена: %v", err)
	}
}

func rpcErr(code, msg string) *RPCError {
	if strings.HasPrefix(msg, "E_") || strings.HasPrefix(code, "E_") {
		if code == "" {
			for _, known := range []string{"E_PLUGIN_OUTSIDE_ROOT", "E_FILE_REF_OUTSIDE_ROOT", "E_FILE_REF_UNCHECKED", "E_NETWORK_DENIED", "E_PLUGIN_LOAD"} {
				if strings.HasPrefix(msg, known+":") {
					code = known
					break
				}
			}
		}
		return &RPCError{Code: -32000, Message: msg, Data: map[string]string{"code": code}}
	}
	return &RPCError{Code: -32602, Message: msg}
}

// callTool — диспетчер инструментов. call связывает вызов с id запроса, чтобы
// notifications/cancelled могла его найти; nil означает «вызов без адреса»
// (тесты, внутренние вызовы) — такой вызов отменить нотификацией нельзя.
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

func (s *Server) runStatus(runID string) string {
	s.mu.Lock()
	st, ok := s.runs[runID]
	s.mu.Unlock()
	if ok {
		select {
		case <-st.done:
			s.mu.Lock()
			defer s.mu.Unlock()
			return st.status
		default:
		}
	}
	// Ран мог быть запущен другим процессом (или до перезапуска сервера), поэтому
	// журнал — источник истины, а не память процесса. Раньше здесь был ранний
	// return "unknown", из-за которого get_run отдавал "unknown" вместе с
	// полным списком событий.
	dir, err := journal.SafeRunDir(s.runsDir, runID)
	if err != nil {
		return "unknown"
	}
	// Журнала нет — такого рана не существует. Без этой проверки нечитаемый
	// каталог выглядел бы как "running", и get_run на выдуманный run_id сообщал
	// бы, что ран идёт.
	if _, err := os.Stat(filepath.Join(dir, "journal.jsonl")); err != nil {
		return "unknown"
	}
	// P2 F-03: статус определяется последними событиями журнала (run_end/
	// run_failed/последний gate_wait), поэтому хватает хвоста в пределах
	// потолка, а не разбора всего журнала в память.
	res, err := journal.NewReader(dir).EventsBounded(0, runJournalLimits(true))
	if err != nil {
		return "running"
	}
	pending := false
	for _, e := range res.Events {
		switch e["type"] {
		case "gate_wait":
			pending = true
		case "gate_decision":
			pending = false
		case "run_end":
			// abort в run_end означает провал: агент не должен увидеть "done"
			// у рана, где прерваны шаги. Для foreach это штатная фильтрация, но
			// из журнала её не видно — склоняемся к осторожности и не врём про успех.
			status := "done"
			if n, _ := e["aborted"].(float64); n > 0 {
				status = "failed"
			}
			s.recordRunStatus(runID, status, "")
			return status
		case "run_cancelled":
			s.recordRunStatus(runID, "cancelled", "")
			return "cancelled"
		case "run_failed":
			msg, _ := e["error"].(string)
			s.recordRunStatus(runID, "failed", msg)
			return "failed"
		}
	}
	if pending {
		return "waiting_human"
	}
	return "running"
}

// recordRunStatus — запомнить статус рана, полученный из журнала. Запись может
// отсутствовать (ран запущен другим процессом), поэтому создаём её, иначе
// обращение к st было бы разыменованием nil.
func (s *Server) recordRunStatus(runID, status, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.runs[runID]
	if !ok {
		st = &runState{id: runID, done: make(chan struct{})}
		s.runs[runID] = st
	}
	st.status = status
	if errMsg != "" {
		st.errMsg = errMsg
	}
}

// runJournalLimits — потолок окна журнала для инструментов. tail=true —
// последние события (статус рана), иначе окно от позиции since (get_run).
func runJournalLimits(tail bool) journal.ReadLimits {
	return journal.ReadLimits{
		MaxBytes:  maxRunJournalBytes,
		MaxEvents: maxRunJournalEvents,
		Tail:      tail,
	}
}

func truncateJSON(v interface{}, limit int) interface{} {
	b, _ := json.Marshal(v)
	if len(b) <= limit {
		return v
	}
	return common.Truncate(string(b), limit) + "(обрезано до 20KB)"
}

func (s *Server) toolGetRun(args map[string]interface{}) (string, bool, *RPCError) {
	runID, _ := args["run_id"].(string)
	if runID == "" {
		return "", false, rpcErr("", "нужен run_id")
	}
	dir, err := journal.SafeRunDir(s.runsDir, runID)
	if err != nil {
		return "", false, rpcErr("", "небезопасный run_id")
	}
	s.mu.Lock()
	_, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		// может, ран из прошлой сессии процесса? проверяем папку
		if _, err := os.Stat(dir); err != nil {
			return "", false, rpcErr("", "ран не найден: "+runID)
		}
	}
	since := 0
	if f, ok := args["since"].(float64); ok {
		since = int(f)
	}
	rd := journal.NewReader(dir)
	// P2 F-03: окно событий ограничено; total/first/next/truncated описывают
	// границы окна, поэтому догрузка по since работает как раньше.
	res, err := rd.EventsBounded(since, runJournalLimits(false))
	if err != nil {
		return "", false, rpcErr("", err.Error())
	}
	status := s.runStatus(runID)
	out := map[string]interface{}{
		"run_id": runID, "status": status, "total": res.Total, "events": res.Events,
		"first": res.First, "next": res.Next, "truncated": res.Truncated,
	}
	s.mu.Lock()
	if rs, ok := s.runs[runID]; ok {
		select {
		case <-rs.done:
			out["stats"] = map[string]int{"ok": rs.okItems, "aborted": rs.aborted}
			if rs.errMsg != "" {
				out["error"] = rs.errMsg
				out["code"] = rs.code
			}
		default:
		}
	}
	s.mu.Unlock()
	if status == "waiting_human" {
		// gate_wait ищется по хвосту журнала, а не по окну от since: иначе
		// догружающий агент (since>0) потерял бы подсказку о гейте
		var lastWait map[string]interface{}
		if tail, tailErr := journal.NewReader(dir).EventsBounded(0, runJournalLimits(true)); tailErr == nil {
			for _, e := range tail.Events {
				if e["type"] == "gate_wait" {
					lastWait = e
				}
			}
		}
		if lastWait != nil {
			out["pending_gate"] = map[string]interface{}{
				"step": lastWait["step"], "form": lastWait["form"], "actions": lastWait["actions"],
			}
			hint := "попросите пользователя одобрить шаг в окне wedra (у агента нет инструмента одобрения)"
			if s.human != nil {
				if u := s.human.PublicURL(runID); u != "" {
					hint += "; консоль: " + u + " (вкладка уже открыта у человека)"
				}
			}
			out["hint"] = hint
		}
	}
	if snap, err := rd.ContextSnapshot(); err == nil && snap != nil {
		out["outputs"] = truncateJSON(snap, 20*1024)
	}
	// secrets никогда не попадают: в снапшоте их нет (только input/steps)
	return toJSON(out), false, nil
}

func (s *Server) toolCancel(args map[string]interface{}) (string, bool, *RPCError) {
	runID, _ := args["run_id"].(string)
	if runID == "" {
		return "", false, rpcErr("", "нужен run_id")
	}
	if _, err := journal.SafeRunDir(s.runsDir, runID); err != nil {
		return "", false, rpcErr("", "небезопасный run_id")
	}
	// Отмена одна на два входа: cancel_run (инструмент) и
	// notifications/cancelled (нотификация клиента). Разные реализации
	// разошлись бы на первой же правке гонки с регистрацией cancel().
	if rpcErr := s.cancelRun(runID); rpcErr != nil {
		return "", false, rpcErr
	}
	return toJSON(map[string]interface{}{"run_id": runID, "status": "cancelling"}), false, nil
}

// MaxConcurrentAgentExec — сколько запусков плагина агентом может идти
// одновременно. exec_plugin выполняется СИНХРОННО и держит процесс до
// maxAgentExecTimeout (300 с), поэтому без потолка агент занимал сервер
// неограниченным числом вызовов. Потолок меньше MaxParallelWidth=32 и
// MaxConcurrentBranches=32: там ограничение на число ветвей пайплайна, здесь —
// на число ОДНОВРЕМЕННЫХ ВНЕШНИХ ПРОЦЕССОВ, живущих до пяти минут.
const MaxConcurrentAgentExec = 4

// maxAgentExecTimeout — потолок длительности одного вызова, секунды.
const maxAgentExecTimeout = 300

// acquireExecSlot — занять слот или отказать. Отказ, а не ожидание в очереди:
// иначе вызовы копятся и MCP-сервер перестаёт отвечать на всё подряд.
func (s *Server) acquireExecSlot() *RPCError {
	if s.execSem == nil {
		return rpcErr("E_AGENT_EXEC_BUSY", "внутренняя ошибка: слоты запусков не инициализированы")
	}
	select {
	case s.execSem <- struct{}{}:
		return nil
	default:
		return rpcErr("E_AGENT_EXEC_BUSY",
			fmt.Sprintf("достигнут предел параллельных запусков (%d): дождитесь окончания текущих", MaxConcurrentAgentExec))
	}
}

func (s *Server) releaseExecSlot() {
	if s.execSem == nil {
		return
	}
	select {
	case <-s.execSem:
	default:
	}
}

func (s *Server) toolExecPlugin(call *inflightCall, args map[string]interface{}) (string, bool, *RPCError) {
	// Проверка разрешения ПЕРВОЙ, до разбора аргументов и до загрузки
	// манифеста: без явного согласия ядра агент не запускает код вообще.
	// Раньше этой проверки не было, и exec_plugin был доступен всегда.
	if !s.trust.AgentCanExec {
		return "", false, rpcErr("E_AGENT_EXEC_DENIED",
			"запуск плагинов агентом запрещён политикой (нужен флаг --allow-agent-exec)")
	}
	// Слот берём до любой работы с файлами: иначе отказ по пределу приходил бы
	// уже после чтения манифеста, и мы гоняли бы диск ради отказа.
	if busy := s.acquireExecSlot(); busy != nil {
		return "", false, busy
	}
	defer s.releaseExecSlot()
	pluginRef, _ := args["plugin"].(string)
	if pluginRef == "" {
		return "", false, rpcErr("", "нужен plugin")
	}
	if err := s.checkPluginRef(pluginRef); err != nil {
		return "", false, rpcErr("E_PLUGIN_OUTSIDE_ROOT", err.Error())
	}
	m, err := s.multi.LoadManifest(pluginRef)
	if err != nil {
		return "", false, rpcErr("", err.Error())
	}
	if err := s.checkPluginNetwork(m); err != nil {
		return "", false, rpcErr("E_NETWORK_DENIED", err.Error())
	}
	// Плагин, написанный агентом, всегда untrusted по построению. Здесь это
	// превращается в требование песочницы, а не в пометку.
	if plugin.IsAgentWrittenPlugin(m) && !s.trust.AllowUntrusted {
		return "", false, rpcErr("E_AGENT_PLUGIN_UNTRUSTED",
			"плагин написан агентом и не помечен как доверенный: передайте --allow-untrusted-plugins")
	}
	// Строгое правило, мимо которого агент не пройдёт.
	//
	// exec_plugin идёт вне пайплайна, поэтому human_gate к нему приделать
	// технически нельзя: ждать некого, состояния ожидания у инструмента нет.
	// Значит запрещаем ровно то, что гейт защищал бы, — плагины с объявленными
	// правами. Обойти это агент не может по построению: он говорит с ядром
	// только через MCP, а решение принимает код.
	//
	// Сеть уже закрыта строкой выше (E_NETWORK_DENIED), здесь остаются запись
	// на диск, чтение секретов и непрочитанный манифест (fail-closed: незнание
	// прав — не доказательство их отсутствия).
	//
	// Проверка стоит ПОСЛЕ проверки доверия, а не до неё: для плагина агента
	// «недоверен» — причина фундаментальнее, и подсказка «запусти через
	// run_pipeline с гейтом» была бы неверной на хосте без изолятора, где
	// такой плагин не запустится вообще.
	//
	// Чего правило НЕ делает: оно не измеряет поведение кода. Плагин, заявивший
	// filesystem: none, всё равно может писать правами пользователя. Против
	// врущего манифеста работает другое — изолятор для недоверенных.
	if caps := pipeline.ManifestCapabilities(m); caps.Dangerous() {
		return "", false, rpcErr("E_AGENT_EXEC_CAPABILITIES",
			"exec_plugin не запускает плагины с объявленными правами ("+caps.Why()+
				"): инструмент идёт мимо пайплайна, а значит мимо human_gate. "+
				"Такой плагин запускается только через run_pipeline с core/human_gate перед ним")
	}
	inputJSON := []byte("{}")
	if input, ok := args["input"].(map[string]interface{}); ok {
		if raw, err := json.Marshal(input); err == nil {
			inputJSON = raw
		}
	}
	timeout := 60.0
	if raw, present := args["timeout_seconds"]; present {
		t, ok := raw.(float64)
		// Как wait_seconds выше: неверное значение — явная ошибка, а не тихое
		// значение по умолчанию. Раньше timeout_seconds=1000, 0, -5, строка или
		// NaN молча превращались в 60, и агент думал, что получил запрошенное.
		if !ok || math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 || t > maxAgentExecTimeout {
			return "", false, rpcErr("", fmt.Sprintf("timeout_seconds должен быть больше 0 и не больше %d секунд", maxAgentExecTimeout))
		}
		timeout = t
	}
	extraEnv := []string{"WEDRA_NETWORK=deny"}
	// Именно ExecWithEnvCtx, а не ExecWithEnv: политика обязана дойти до
	// enforceTrust. ExecWithEnv подставляет context.Background(), то есть
	// нулевую политику, и флаги --allow/--deny-untrusted-plugins не влияли
	// на запуск из MCP.
	// exec_plugin синхронный и может держать процесс до 300 секунд, поэтому
	// вызов отменяем: notifications/cancelled обрывает запуск, как и ран.
	execCtx, cancelExec := context.WithCancel(plugin.WithTrustPolicy(context.Background(), s.trust))
	defer cancelExec()
	s.bindExec(call, cancelExec)
	ctx := execCtx
	// Намерение пишется ДО запуска. Решение агента о запуске кода — событие
	// доверия, и оно обязано быть в журнале даже тогда, когда процесс
	// умирает на середине: иначе выполнение внешнего кода остаётся вообще
	// без следа. Ошибка записи здесь означает настоящий отказ — плагин не
	// запускается.
	execID, auditErr := s.auditAgentExecIntent(pluginRef, m, timeout)
	if auditErr != nil {
		return "", false, rpcErr("E_AGENT_EXEC_AUDIT", auditErr.Error())
	}
	started := time.Now()
	// Прогресс до запуска: единственная точка, где мы ещё можем что-то
	// сказать — сам ExecWithEnvCtx блокирует вызов до конца процесса.
	s.notifyProgress(call, 0, timeout, "плагин "+pluginRef+" запущен (таймаут "+strconv.FormatFloat(timeout, 'f', 0, 64)+" с)")
	// Дробные секунды — точно, а не усечением: time.Duration(timeout) отрезал
	// бы 1.5с до 1с, а 0.5с — до нуля, то есть до мгновенного таймаута.
	res := plugin.ExecWithEnvCtx(ctx, m, inputJSON, time.Duration(timeout*float64(time.Second)), extraEnv)
	// Исход дописывается после. Его потеря не отменяет уже состоявшееся
	// исполнение, поэтому здесь только отказ вернуть результат: в журнале
	// всё равно остаётся намерение с тем же exec_id.
	if auditErr := s.auditAgentExecResult(execID, pluginRef, m, res, time.Since(started)); auditErr != nil {
		return "", false, rpcErr("E_AGENT_EXEC_AUDIT", auditErr.Error())
	}
	if res.Platform {
		return "", false, &RPCError{Code: -32000, Message: res.ErrMsg, Data: map[string]string{"code": res.ErrCode}}
	}
	if res.ExitCode != 0 {
		return "", false, &RPCError{Code: -32000, Message: res.ErrMsg, Data: map[string]string{"code": res.ErrCode}}
	}
	out, dropped, err := plugin.EnforceOutput(m, res.Output)
	if err != nil {
		return "", false, &RPCError{Code: -32000, Message: err.Error(), Data: map[string]string{"code": "contract_output"}}
	}
	if len(dropped) > 0 {
		return toJSON(map[string]interface{}{"output": out, "dropped": dropped}), false, nil
	}
	return toJSON(map[string]interface{}{"output": out}), false, nil
}

// agentExecAuditFile — append-only журнал запусков плагинов агентом. Отдельный
// от журналов ранов: exec_plugin не создаёт ран, но это решение о доверии, и
// оно должно читаться так же надёжно, как gate_decision.
const agentExecAuditFile = "agent-exec.jsonl"

// События журнала. Их два на один запуск, и порядок обязателен: сначала
// намерение, потом результат.
const (
	// agentExecEventIntent — запись ДО запуска плагина. Именно она даёт
	// настоящую гарантию: не записалось намерение — плагин не запускается.
	agentExecEventIntent = "agent_plugin_exec_intent"
	// agentExecEventResult — запись ПОСЛЕ запуска: чем закончилось.
	agentExecEventResult = "agent_plugin_exec"
)

// newExecID связывает пару записей одного запуска. Читатель журнала видит
// намерение без результата (значит процесс упал или был убит) и результат без
// намерения (быть не должно — это признак дырки в журнале).
func newExecID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("сгенерировать id запуска: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// writeAuditRecord дописывает одну строку в журнал запусков плагинов агентом.
// Общий низ для обеих записей: иначе «не записалось» и «не сериализовалось»
// разъезжаются.
func (s *Server) writeAuditRecord(record map[string]interface{}) error {
	return s.writeAuditFile(agentExecAuditFile, record)
}

// writeAuditFile — дописать запись в указанный append-only журнал каталога ранов.
// Общий низ для журналов безопасности: fsync обязателен в обоих, потому что
// запись должна пережить падение процесса сразу после действия.
func (s *Server) writeAuditFile(name string, record map[string]interface{}) error {
	if s.runsDir == "" {
		return fmt.Errorf("не задан каталог ранов, запись о запуске агентом невозможна")
	}
	record["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("не сериализовать запись аудита: %w", err)
	}
	path := filepath.Join(s.runsDir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("открыть журнал запусков агента (%s): %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("записать в журнал запусков агента (%s): %w", path, err)
	}
	// Синхронизируем файл: намерение обязано оказаться на диске ДО запуска
	// плагина. При вылете процесса иначе буфер ушёл бы в никуда, и мы
	// получили бы ровно то, ради чего переставили записи: запуск без следа.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("сбросить журнал запусков агента на диск (%s): %w", path, err)
	}
	return nil
}

// logf — единственный канал сообщений сервера в лог. В MCP-процессе stdout
// принадлежит протоколу (cli/mcp.go перенаправляет os.Stdout в stderr), поэтому
// лог всегда в stderr и никогда не попадает в JSON-RPC.
func (s *Server) logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "wedra mcp: "+format+"\n", args...)
}

// auditUntrusted — что писать в журнал аудита как "untrusted".
//
// Раньше здесь стоял IsAgentWrittenPlugin(m), и это было равносильно верно:
// внешним кодом был ровно плагин из agent-plugins/. После инверсии доверия
// (H1) внешним стал ЛЮБОЙ плагин, которого нет в allow-list, — и запись
// IsAgentWrittenPlugin дала бы в журнале безопасности "untrusted": false для
// кода, который на самом деле ушёл в песочницу. Журнал, в котором враньё,
// хуже отсутствия журнала: на него смотрят при разборе инцидента.
func (s *Server) auditUntrusted(m *pipeline.Manifest) bool {
	return !plugin.IsTrusted(m, s.trust)
}

// auditAgentExecIntent фиксирует НАМЕРЕНИЕ запустить плагин и возвращает id
// запуска. Вызывается до исполнения, и её ошибка означает отказ: код не
// запускается вовсе.
//
// Почему именно до запуска, а не после. При записи после падения процесса
// посреди исполнения не остаётся НИЧЕГО: плагин уже отработал, а след — нет.
// Запись до запуска закрывает и это, и даёт настоящий запрет, а не
// «результат не отдан».
func (s *Server) auditAgentExecIntent(pluginRef string, m *pipeline.Manifest, timeout float64) (string, error) {
	execID, err := newExecID()
	if err != nil {
		return "", err
	}
	err = s.writeAuditRecord(map[string]interface{}{
		"event":     agentExecEventIntent,
		"exec_id":   execID,
		"source":    "agent_auto",
		"plugin":    pluginRef,
		"plugin_id": m.ID,
		"untrusted": s.auditUntrusted(m),
		"timeout":   timeout,
	})
	if err != nil {
		return "", err
	}
	return execID, nil
}

// auditAgentExecResult дописывает исход запуска. Ошибка здесь означает
// отказ вернуть результат агенту (E_AGENT_EXEC_AUDIT), но плагин к этому
// моменту уже отработал — намерение о нём в журнале уже есть.
func (s *Server) auditAgentExecResult(execID, pluginRef string, m *pipeline.Manifest, res *plugin.ExecResult, took time.Duration) error {
	return s.writeAuditRecord(map[string]interface{}{
		"event":     agentExecEventResult,
		"exec_id":   execID,
		"source":    "agent_auto",
		"plugin":    pluginRef,
		"plugin_id": m.ID,
		"untrusted": s.auditUntrusted(m),
		"exit_code": res.ExitCode,
		"err_code":  res.ErrCode,
		"duration":  took.String(),
	})
}

func (s *Server) checkPluginNetwork(m *pipeline.Manifest) error {
	for _, permission := range m.Permissions.Network {
		if permission.AnyHost {
			return fmt.Errorf("E_NETWORK_DENIED: плагин %s заявил any_host в exec_plugin", m.ID)
		}
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(permission.Host), "."))
		if host == "" {
			return fmt.Errorf("E_NETWORK_DENIED: пустой host в permissions плагина %s", m.ID)
		}
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
			return fmt.Errorf("E_NETWORK_DENIED: host %q запрещён в exec_plugin", permission.Host)
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
			return fmt.Errorf("E_NETWORK_DENIED: private/loopback host %q запрещён в exec_plugin", permission.Host)
		}
	}
	return nil
}
