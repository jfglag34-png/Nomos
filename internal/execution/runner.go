package execution

import (
	stdctx "context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"github.com/wykserdex/wedra/internal/gate"
	"github.com/wykserdex/wedra/internal/journal"
	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
	"github.com/wykserdex/wedra/internal/runctx"
)


type RunOptions struct {
	Yes     bool
	RunsDir string
	Quiet   bool
	Resume  string
	Store   string
	DBPath  string
	// v0.24: GUI — ID рана известен до старта (ответ API, карточка гейта)
	RunID string
	// v0.24: фабрика не-терминального ввода гейта (GUI/API). Вызывается на
	// каждом встроенном gate-шаге; nil — терминальный stdin (дефолт).
	//
	// Жизненный цикл UI из фабрики принадлежит вызывающему, а не рану:
	// Server.clearGate/cancel закрывает ChannelUI сам, ран не трогает чужой
	// ввод (у API их по одному на гейт, и они переживают конец шага).
	GateUI func(*pipeline.Step) gate.GateUI
	// Agent-track policy: --yes never approves gates for MCP runs.
	NoAutoApprove bool
	// Agent-track policy: stdin/stdout are occupied by JSON-RPC, so a
	// terminal gate is forbidden without GateUI.
	MCPMode bool
	// Agent-track cancellation context (Ctrl+C, API /cancel, MCP cancel_run).
	Ctx stdctx.Context
	// DenyUntrusted — политика доверия: запретить запуск внешнего кода
	// (плагинов без явного доверия ядра). Fail-closed, включается флагом
	// --deny-untrusted-plugins.
	DenyUntrusted bool
	// AllowUntrusted — согласие запускать плагины с sandbox: untrusted.
	// Они уходят в изолятор (bwrap/sandbox-exec); если изолятора нет,
	// запуск падает, а не выполняется без песочницы.
	AllowUntrusted bool
	// Trusted — allow-list доверенных плагинов оператора. Единственный
	// источник доверия: пустой/nil означает «не доверен никто», и такой ран
	// честно падает на первом же внешнем плагине.
	Trusted *plugin.AllowList
	// loopBudget — общий счётчик итераций loop на весь ран. Заполняется в
	// runWithStore; nil обрабатывается как «бюджет не заведён» (см.
	// RunOptions.consumeLoopIteration), чтобы внешние вызовы Run с нулевой
	// структурой не падали.
	//
	// Нужен именно ран, а не шаг: MaxTotalLoopBudget проверялся счётчиком,
	// который обнулялся на каждый вызов runStepLoop, то есть десять loop-шагов
	// по 1000 итераций давали 10 000 запусков плагина, хотя имя константы
	// обещало «глобальный».
	loopBudget *loopBudgetCounter

	// gateUI — терминальный ввод гейтов на весь ран. Указатель по той же
	// причине, что и loopBudget: опции копируются по значению на каждом
	// шаге, а экземпляр ввода обязан быть общим.
	//
	// Раньше терминальный UI создавался в runStep на КАЖДЫЙ гейт
	// (gate.NewService() → NewStdinUI() → bufio.NewReader(os.Stdin)).
	// При TTY это не видно, а при stdin из pipe первый гейт забирал в буфер
	// весь ввод разом, и второй получал EOF: скриптованные прогоны с
	// несколькими гейтами не работали. Чинить перечитыванием stdin нельзя —
	// потерянный остаток уже не вернуть; нужен один буфер на прогон.
	//
	// Владелец — ран, и он его НЕ закрывает: os.Stdin принадлежит процессу,
	// закрывать его нечем и нельзя (сломались бы следующий ран и любой
	// другой читатель stdin). Закрывать требуется UI из GateUI — это зона
	// вызывающего, см. поле выше.
	gateUI gate.GateUI
}

// loopBudgetCounter — счётчик итераций loop на ран. Указатель в RunOptions:
// опции копируются по значению на каждом шаге, и счётчик обязан быть обным.
//
// limit — потолок для тестов. Ноль означает боевой MaxTotalLoopBudget;
// подставлять маленький лимит в проде нечем, поле непубличное.
var ErrCancelled = errors.New("ран отменён (cancelled)")

// RunError — ошибка рантайма с кодом из protocol/v0.2/ERRORS.md
// (contract_output, platform:<code>, when_error, foreach_path, secrets_missing,
// network_denied, cancelled, validation_failed, run_error). Код пишется в run_failed/step_failed.
type RunError struct {
	Code string
	Err  error
}

func (e *RunError) Error() string { return e.Err.Error() }
func (e *RunError) Unwrap() error { return e.Err }

func runErr(code string, format string, a ...interface{}) error {
	return &RunError{Code: code, Err: fmt.Errorf(format, a...)}
}

// ErrorCode — код ошибки рантайма (для журнала, API и MCP).
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrCancelled) {
		return "cancelled"
	}
	var re *RunError
	if errors.As(err, &re) {
		return re.Code
	}
	return "run_error"
}

// failEvent — терминальное событие рана: run_cancelled для отмены,
// иначе run_failed; оба с полем code. Затем snapshot (resume).
func (o RunOptions) cancelled() bool {
	return o.Ctx != nil && o.Ctx.Err() != nil
}

func (o RunOptions) logf(format string, a ...interface{}) {
	if !o.Quiet {
		fmt.Printf(format+"\n", a...)
	}
}

type RunStats struct {
	OK      int
	Aborted int
	RunDir  string
	// P2 F-04: снапшоты контекста, потерянные до этого рана (при --resume
	// журнал общий). Ненулевое значение означает, что context.json отстаёт от
	// курсора resume, и терминальный статус рана — не «ок».
	SnapshotLosses int
}

func resumeCursor(dir string) (int, RunStats, error) {
	events, err := journal.NewReader(dir).Events()
	if err != nil {
		return 0, RunStats{}, err
	}
	latest := map[int]string{}
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		if typ != "item_start" && typ != "item_end" {
			continue
		}
		idx, ok := journal.ParseItemIndex(ev["item_index"])
		if !ok {
			continue
		}
		if typ == "item_end" {
			status, _ := ev["status"].(string)
			latest[idx] = status
		}
	}
	var stats RunStats
	for idx := 0; ; idx++ {
		status, ended := latest[idx]
		if !ended {
			return idx, stats, nil
		}
		if status == "" || status == "ok" {
			stats.OK++
			continue
		}
		return idx, stats, nil
	}
}

func pipelineIdentity(pf *pipeline.PipelineFile) (string, error) {
	raw, err := json.Marshal(pf)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:]), nil
}

func journalPipelineIdentity(dir string) (string, error) {
	events, err := journal.NewReader(dir).Events()
	if err != nil {
		return "", err
	}
	identity := ""
	for _, event := range events {
		if event["type"] != "run_start" {
			continue
		}
		candidate, ok := event["pipeline_hash"].(string)
		if !ok || candidate == "" {
			return "", fmt.Errorf("journal run_start без pipeline_hash")
		}
		if identity != "" && identity != candidate {
			return "", fmt.Errorf("journal содержит разные pipeline_hash")
		}
		identity = candidate
	}
	if identity == "" {
		return "", fmt.Errorf("journal не содержит run_start")
	}
	return identity, nil
}

func writeAggregates(ctx *runctx.Ctx, agg map[string][]interface{}, steps []*pipeline.Step) {
	stepsMap, ok := ctx.Data["steps"].(map[string]interface{})
	if !ok {
		return
	}
	for _, st := range steps {
		if st.Foreach != "" {
			continue
		}
		if values, ok := agg[st.ID]; ok {
			stepsMap[st.ID+"_all"] = values
		}
	}
}

var cyrillicSanitize = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "yo", 'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
	'А': "A", 'Б': "B", 'В': "V", 'Г': "G", 'Д': "D", 'Е': "E", 'Ё': "Yo", 'Ж': "Zh", 'З': "Z", 'И': "I", 'Й': "Y", 'К': "K", 'Л': "L", 'М': "M", 'Н': "N", 'О': "O", 'П': "P", 'Р': "R", 'С': "S", 'Т': "T", 'У': "U", 'Ф': "F", 'Х': "H", 'Ц': "C", 'Ч': "Ch", 'Ш': "Sh", 'Щ': "Sch", 'Ъ': "", 'Ы': "Y", 'Ь': "", 'Э': "E", 'Ю': "Yu", 'Я': "Ya",
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if replacement, ok := cyrillicSanitize[r]; ok {
			b.WriteString(replacement)
			continue
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	result := strings.Trim(b.String(), "-")
	if result == "" {
		return "run"
	}
	return result
}

func NewRunID(name string) (string, error) {
	var entropy [6]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + sanitize(name) + "-" + hex.EncodeToString(entropy[:]), nil
}

// Sanitize — публичная обёртка (v0.24: GUI генерирует runID тем же форматом).
func Sanitize(s string) string { return sanitize(s) }

type Engine interface {
	LoadManifest(ref string) (*pipeline.Manifest, error)
}

func validateRun(pf *pipeline.PipelineFile, eng Engine) error {
	issues := pipeline.ValidateIssues(pf, eng)
	var messages []string
	for _, issue := range issues {
		if issue.Severity != pipeline.SeverityError || runtimeDataIssue(issue.Code) {
			continue
		}
		messages = append(messages, issue.Message)
	}
	if len(messages) > 0 {
		return runErr("validation_failed", "pipeline validation failed: %s", strings.Join(messages, "; "))
	}
	return nil
}

func runtimeDataIssue(code string) bool {
	switch code {
	case pipeline.E_PORT_UNBOUND, pipeline.E_PORT_SOURCE, pipeline.E_TYPE_MISMATCH, pipeline.E_FORMAT_INPUT, pipeline.E_FORMAT_MISMATCH, pipeline.E_OPTIONAL_REQUIRED:
		return true
	default:
		return false
	}
}

func Run(pf *pipeline.PipelineFile, eng Engine, opts RunOptions) (RunStats, error) {
	if err := validateRun(pf, eng); err != nil {
		return RunStats{}, err
	}
	if opts.Store == "json" {
		s := journal.NewJsonStore(opts.RunsDir, opts.DBPath)
		return runWithStore(pf, eng, opts, s)
	}
	return runWithStore(pf, eng, opts, journal.NewFilesystemStore(opts.RunsDir))
}

func runWithStore(pf *pipeline.PipelineFile, eng Engine, opts RunOptions, store journal.RunStore) (RunStats, error) {
	var stats RunStats
	if opts.Ctx == nil {
		opts.Ctx = stdctx.Background()
	}
	// Бюджет итераций loop на весь ран. Ставится здесь, а не в Run, чтобы
	// resume тоже начинался с нуля: budget прошлого рана — его история, а
	// лимит текущего рана — нет.
	if opts.loopBudget == nil {
		opts.loopBudget = &loopBudgetCounter{}
	}
	// Терминальный ввод гейтов — один на весь ран. Читать из os.Stdin
	// можно только одним буфером: bufio.Reader забирает из дескриптора
	// столько, сколько там есть, и остаток живёт в нём, а не в pipe.
	// Новый reader на каждый гейт отдавал второму гейту EOF (обход TTY).
	//
	// Заводится здесь, а не в runStep, ровно по той же причине, что и
	// loopBudget: опции копируются по значению, общий экземпляр обязан
	// быть указателем. Закрывать нечего — os.Stdin не наш (см. RunOptions.gateUI).
	//
	// MCP-рену терминальный ввод запрещён (stdin занят JSON-RPC), поэтому
	// там UI не заводится, а гейт без фабрики падает в runStep.
	if opts.gateUI == nil && opts.GateUI == nil && !opts.MCPMode {
		opts.gateUI = gate.NewStdinUI()
	}
	// Политика доверия к коду плагинов задаётся ядром (allow-list оператора +
	// --deny-untrusted-plugins / --allow-untrusted-plugins) и наследуется всеми
	// шагами через ctx; плагин не может её ослабить.
	opts.Ctx = plugin.WithTrustPolicy(opts.Ctx, plugin.TrustPolicy{
		DenyUntrusted:  opts.DenyUntrusted,
		AllowUntrusted: opts.AllowUntrusted,
		Trusted:        opts.Trusted,
	})
	if pf.Pipeline.Network != "" && pf.Pipeline.Network != "allow" && pf.Pipeline.Network != "deny" {
		return stats, runErr("network_policy", "pipeline network=%q: допускаются allow или deny", pf.Pipeline.Network)
	}
	// v0.16: secrets — до любого эффекта: не запустим ран без ключей
	var missingSecrets []string
	for _, k := range pf.Pipeline.Secrets {
		if os.Getenv(k) == "" {
			missingSecrets = append(missingSecrets, k)
		}
	}
	if len(missingSecrets) > 0 {
		return stats, runErr("secrets_missing", "secrets: не заданы переменные окружения: %s (export перед запуском, значения в YAML не живут)", strings.Join(missingSecrets, ", "))
	}
	// v0.17: network: deny — до любого эффекта, не доверяем validate.
	// Поле не задано — тоже deny: задокументированный дефолт не должен
	// отличаться от фактического, иначе пайплайн без network прогоняет
	// плагин с заявленной сетью вообще без гейта.
	netPolicy := pipeline.EffectiveNetwork(&pf.Pipeline)
	if netPolicy == pipeline.NetworkDeny {
		for i := range pf.Pipeline.Steps {
			st := &pf.Pipeline.Steps[i]
			if plugin.IsBuiltin(st.Plugin) {
				continue
			}
			if plugin.IsBuiltinNamespace(st.Plugin) {
				return stats, runErr("E_PLUGIN_LOAD", "шаг %s: неизвестный встроенный модуль: %s", st.ID, st.Plugin)
			}
			m, err := eng.LoadManifest(st.Plugin)
			if err != nil {
				continue // ошибка резолвинга всплывает ниже
			}
			if len(m.Permissions.Network) > 0 {
				why := "пайплайн запрещает (network: deny)"
				if pf.Pipeline.Network == "" {
					why = "поле network не задано, а пустое поле означает deny — укажите network: allow, чтобы разрешить"
				}
				return stats, runErr("network_denied", "network: шаг %s (плагин %s) заявил сеть (%s), а %s", st.ID, st.Plugin, pipeline.NetworkHosts(m), why)
			}
		}
	} else if netPolicy == pipeline.NetworkAllow {
		// network: allow — пайплайн разрешил сеть. Но исполнимо только явное
		// any_host: true. Список конкретных хостов выглядит как ограничение,
		// поэтому превращать его в полный доступ молча нельзя: точечный фильтр
		// не реализован, значит честный ответ — отказать и объяснить.
		//
		// Ветка == allow, а не «иначе»: сеть выдаётся только явным решением
		// пайплайна, поле не задано — это deny (см. EffectiveNetwork).
		for i := range pf.Pipeline.Steps {
			st := &pf.Pipeline.Steps[i]
			if plugin.IsBuiltin(st.Plugin) || plugin.IsBuiltinNamespace(st.Plugin) {
				continue
			}
			m, err := eng.LoadManifest(st.Plugin)
			if err != nil {
				continue
			}
			if pipeline.NetworkHasStructuredDeclarations(m) {
				return stats, runErr("network_not_enforceable",
					"network: шаг %s (плагин %s) заявил сеть по host:port (%s), но точечный egress-фильтр не реализован — полный доступ вместо списка хостов выдавать нельзя. Либо уберите сеть, либо объявите any_host: true, если действительно нужен весь интернет",
					st.ID, st.Plugin, pipeline.NetworkHosts(m))
			}
		}
	}
	if opts.RunsDir == "" {
		opts.RunsDir = journal.DefaultRunsDir()
	}
	if fs, ok := store.(*journal.FilesystemStore); ok {
		if fs.BaseDir == "" {
			fs.BaseDir = opts.RunsDir
		} else {
			opts.RunsDir = fs.BaseDir
		}
	}
	if sq, ok := store.(*journal.JsonStore); ok {
		if sq.BaseDir == "" {
			sq.BaseDir = opts.RunsDir
		} else {
			opts.RunsDir = sq.BaseDir
		}
	}

	var ctx *runctx.Ctx
	var startItemIdx int
	var j *journal.Journal
	var err error
	pipelineHash, err := pipelineIdentity(pf)
	if err != nil {
		return stats, runErr("context_serialization", "pipeline identity serialization: %v", err)
	}

	if opts.Resume != "" {
		data, err := store.LoadContext(opts.Resume)
		if err != nil {
			return stats, fmt.Errorf("--resume %s: %w", opts.Resume, err)
		}
		// context.json приходит с диска: проверяем форму до любого исполнения.
		// Без этого steps: 5 (или отсутствующий input) роняли ран panic'ом в
		// SetStep/SetInput — вместо типизированной ошибки.
		data, err = runctx.Normalize(data)
		if err != nil {
			return stats, fmt.Errorf("--resume %s: context.json не той формы: %w", opts.Resume, err)
		}
		ctx = &runctx.Ctx{Data: data}
		j, err = store.OpenAppend(opts.Resume)
		if err != nil {
			return stats, err
		}
		defer j.Close()
		journalHash, err := journalPipelineIdentity(j.Dir)
		if err != nil {
			return stats, fmt.Errorf("--resume %s: %w", opts.Resume, err)
		}
		if journalHash != pipelineHash {
			return stats, fmt.Errorf("--resume %s: pipeline identity mismatch", opts.Resume)
		}
		startItemIdx, priorStats, err := resumeCursor(j.Dir)
		if err != nil {
			return stats, fmt.Errorf("--resume %s: не читается journal: %w", opts.Resume, err)
		}
		// P2 F-04: resume берёт курсор из журнала (item_end), а состояние — из
		// context.json. Если снапшот когда-то терялся, disk-состояние отстаёт от
		// курсора, и «продолжить с max+1» молча скормит следующим шагам неполный
		// контекст. Ран продолжает работу, но терминальный вердикт обязан быть
		// честным (finish).
		if priorLost := priorSnapshotLosses(j.Dir); priorLost > 0 {
			stats.SnapshotLosses = priorLost
			opts.logf("  ! в журнале %d потерянных снапшотов: context.json отстаёт от курсора resume", priorLost)
		}
		stats.RunDir = j.Dir
		stats.OK, stats.Aborted = priorStats.OK, priorStats.Aborted
		opts.logf("▶ resume %q с элемента %d (журнал: %s)", pf.Pipeline.Name, startItemIdx, j.Dir)
		j.Event("run_resumed", map[string]interface{}{"from_item": startItemIdx, "pipeline_hash": pipelineHash})
	} else {
		runID := opts.RunID
		if runID == "" {
			runID, err = NewRunID(pf.Pipeline.Name)
			if err != nil {
				return stats, fmt.Errorf("run id: %w", err)
			}
		}
		// use store.Create so SQLite DB gets entry
		j, err = store.Create(runID)
		if err != nil {
			return stats, err
		}
		defer j.Close()
		stats.RunDir = j.Dir
		opts.logf("▶ запуск %q  (журнал: %s)", pf.Pipeline.Name, j.Dir)
		j.Event("run_start", map[string]interface{}{"pipeline": pf.Pipeline.Name, "pipeline_hash": pipelineHash})
		ctx = runctx.NewCtx(pf.Pipeline.Input)
	}

	var items []interface{}
	var preSteps []*pipeline.Step
	var loopSteps []*pipeline.Step
	var postSteps []*pipeline.Step

	if pf.Pipeline.Foreach == "" {
		items = []interface{}{nil}
		for i := range pf.Pipeline.Steps {
			st := &pf.Pipeline.Steps[i]
			if st.AfterForeach {
				postSteps = append(postSteps, st)
			} else {
				loopSteps = append(loopSteps, st)
			}
		}
	} else if strings.HasPrefix(pf.Pipeline.Foreach, "input.") {
		v, ok := ctx.Get(pf.Pipeline.Foreach)
		if !ok {
			return stats, runErr("foreach_path", "foreach: путь %s не найден", pf.Pipeline.Foreach)
		}
		arr, ok := v.([]interface{})
		if !ok {
			return stats, runErr("foreach_path", "foreach: %s не массив", pf.Pipeline.Foreach)
		}
		items = arr
		for i := range pf.Pipeline.Steps {
			st := &pf.Pipeline.Steps[i]
			if st.AfterForeach {
				postSteps = append(postSteps, st)
			} else {
				loopSteps = append(loopSteps, st)
			}
		}
	} else if strings.HasPrefix(pf.Pipeline.Foreach, "steps.") {
		parts := strings.Split(pf.Pipeline.Foreach, ".")
		if len(parts) < 3 {
			return stats, runErr("foreach_path", "foreach: %s должен быть вида steps.<id>.<field>", pf.Pipeline.Foreach)
		}
		srcID := parts[1]
		srcIdx := -1
		for i, st := range pf.Pipeline.Steps {
			if st.ID == srcID {
				srcIdx = i
				break
			}
		}
		if srcIdx == -1 {
			return stats, runErr("foreach_path", "foreach: шаг %s не найден", srcID)
		}
		for i := 0; i <= srcIdx; i++ {
			preSteps = append(preSteps, &pf.Pipeline.Steps[i])
		}
		for i := srcIdx + 1; i < len(pf.Pipeline.Steps); i++ {
			st := &pf.Pipeline.Steps[i]
			if st.AfterForeach {
				postSteps = append(postSteps, st)
			} else {
				loopSteps = append(loopSteps, st)
			}
		}
		if opts.Resume == "" {
			opts.logf("  → фаза 1: получение массива %s из %d шагов", pf.Pipeline.Foreach, len(preSteps))
			preRefs := append([]*pipeline.Step(nil), preSteps...)
			acts, err := runParallelSegments(eng, pf, preRefs, ctx, j, opts, true)
			if err != nil {
				failEvent(j, ctx, err, nil)
				return stats, fmt.Errorf("pre-foreach: %w", err)
			}
			for _, a := range acts {
				if a == "abort_item" {
					return stats, fmt.Errorf("foreach: pre-фаза остановлена")
				}
			}
			j.Snapshot(ctx)
		}
		v, ok := ctx.Get(pf.Pipeline.Foreach)
		if !ok {
			return stats, runErr("foreach_path", "foreach: после pre-фазы путь %s не найден", pf.Pipeline.Foreach)
		}
		arr, ok := v.([]interface{})
		if !ok {
			return stats, runErr("foreach_path", "foreach: %s не массив (после pre-фазы)", pf.Pipeline.Foreach)
		}
		items = arr
	} else {
		return stats, runErr("foreach_path", "foreach: путь должен начинаться с input. или steps., got %s", pf.Pipeline.Foreach)
	}
	if len(items) > pipeline.MaxForeachItems {
		return stats, runErr("resource_limit", "foreach: %d элементов, максимум %d", len(items), pipeline.MaxForeachItems)
	}

	itemKey := pf.Pipeline.ForeachItem
	if itemKey == "" {
		itemKey = "item"
	}

	agg := map[string][]interface{}{}
	aggregateCount := 0
	if startItemIdx > 0 {
		if stepsMap, ok := ctx.Data["steps"].(map[string]interface{}); ok {
			for _, st := range loopSteps {
				if raw, ok := stepsMap[st.ID+"_all"]; ok {
					if arr, ok := raw.([]interface{}); ok {
						if len(arr) >= startItemIdx {
							agg[st.ID] = append([]interface{}{}, arr[:startItemIdx]...)
						} else {
							agg[st.ID] = append([]interface{}{}, arr...)
						}
					}
				}
			}
		}
	}
	for _, values := range agg {
		aggregateCount += len(values)
	}
	if aggregateCount > pipeline.MaxAggregateItems {
		return stats, runErr("resource_limit", "resume aggregate exceeds %d items", pipeline.MaxAggregateItems)
	}

	if startItemIdx > 0 {
		if startItemIdx >= len(items) {
			opts.logf("  resume: все %d элементов уже пройдены", len(items))
			if len(postSteps) > 0 {
				opts.logf("  → фаза 3: post-foreach %d шагов", len(postSteps))
				for _, st := range postSteps {
					if _, err := runStep(eng, pf, st, ctx, j, opts); err != nil {
						failEvent(j, ctx, err, map[string]interface{}{"step": st.ID})
						return stats, fmt.Errorf("шаг %s (post-foreach): %w", st.ID, err)
					}
				}
			}
			if err := finish(j, stats.SnapshotLosses, map[string]interface{}{"ok": stats.OK, "aborted": stats.Aborted, "resumed": true}); err != nil {
				return stats, err
			}
			return stats, nil
		}
		opts.logf("  resume: пропускаем %d элементов, продолжаем с %d/%d", startItemIdx, startItemIdx+1, len(items))
	}

	for idx, it := range items {
		if idx < startItemIdx {
			continue
		}
		if opts.cancelled() {
			failEvent(j, ctx, ErrCancelled, map[string]interface{}{"item_index": idx})
			return stats, ErrCancelled
		}
		if pf.Pipeline.Foreach != "" {
			if len(preSteps) == 0 {
				ctx.ResetSteps()
			} else {
				if stepsMap, ok := ctx.Data["steps"].(map[string]interface{}); ok {
					for _, st := range loopSteps {
						delete(stepsMap, st.ID)
						delete(stepsMap, st.ID+"_all")
					}
				}
			}
			ctx.SetInput(itemKey, it)
			opts.logf("\n─ [%d/%d] %v", idx+1, len(items), it)
		}
		j.Event("item_start", map[string]interface{}{"item_index": idx, "item": it})

		itemStatus := "ok"
		loopRefs := append([]*pipeline.Step(nil), loopSteps...)
		acts, err := runParallelSegments(eng, pf, loopRefs, ctx, j, opts, true)
		if err != nil {
			failEvent(j, ctx, err, nil)
			return stats, fmt.Errorf("в цикле: %w", err)
		}
		for i, action := range acts {
			st := loopRefs[i]
			if action == "abort_item" {
				itemStatus = "aborted"
				j.Event("item_aborted", map[string]interface{}{"item_index": idx, "at_step": st.ID})
				break
			}
			// v0.20: у шага с foreach агрегируем его внутренний _all, не последний выход
			srcKey := st.ID
			if st.Foreach != "" {
				srcKey = st.ID + "_all"
			}
			if v, ok := ctx.Steps()[srcKey]; ok {
				agg[st.ID] = append(agg[st.ID], v)
				aggregateCount++
				if aggregateCount > pipeline.MaxAggregateItems {
					err := runErr("resource_limit", "aggregate exceeds %d items", pipeline.MaxAggregateItems)
					failEvent(j, ctx, err, nil)
					return stats, err
				}
			}
		}
		if pf.Pipeline.Foreach != "" {
			writeAggregates(ctx, agg, loopSteps)
		}
		// P2 F-04: ошибку здесь ловить нечего — Journal сам пишет событие
		// snapshot_lost и считает потерю, терминальный вердикт принимает
		// finish. Раньше возврат игнорировался, и потеря была не видна нигде.
		j.Snapshot(ctx)
		j.Event("item_end", map[string]interface{}{"item_index": idx, "status": itemStatus})
		if itemStatus == "aborted" {
			stats.Aborted++
		} else {
			stats.OK++
		}
	}

	if len(postSteps) > 0 {
		opts.logf("\n▶ фаза 3: post-foreach %d шагов (агрегаты: %d)", len(postSteps), len(agg))
		writeAggregates(ctx, agg, loopSteps)
		j.Event("post_phase_start", map[string]interface{}{"steps": len(postSteps)})
		postRefs := append([]*pipeline.Step(nil), postSteps...)
		acts, err := runParallelSegments(eng, pf, postRefs, ctx, j, opts, true)
		if err != nil {
			failEvent(j, ctx, err, nil)
			return stats, fmt.Errorf("post-foreach: %w", err)
		}
		for _, a := range acts {
			if a == "abort_item" {
				stats.Aborted++
				break
			}
		}
		j.Snapshot(ctx)
		j.Event("post_phase_end", map[string]interface{}{})
	}

	if err := finish(j, stats.SnapshotLosses, map[string]interface{}{"ok": stats.OK, "aborted": stats.Aborted}); err != nil {
		return stats, err
	}
	opts.logf("\n■ ран завершён: ok=%d aborted=%d → %s", stats.OK, stats.Aborted, j.Dir)
	return stats, nil
}

// stepSegment — фрагмент списка шагов: одиночный шаг или параллельная группа.
