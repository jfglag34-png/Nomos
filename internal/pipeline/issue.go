package pipeline

// Severity — уровень проблемы: error блокирует запуск, warning — нет.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Fix — машинная подсказка для агента: что делать с проблемой.
// Op: "bind" — перепривязать порт на один из Candidates,
//
//	"set" — выставить значение Target,
//	"declare" — объявить недостающее (secrets, поле input).
//
// Target: "steps.<id>.<port>" | "input.<field>" | "pipeline.<field>".
// Candidates: отсортированный список допустимых значений.
type Fix struct {
	Op         string   `json:"op"`
	Target     string   `json:"target,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
}

// Issue — структурная ошибка валидации. Code — стабильный публичный
// контракт (см. protocol/v0.2/ERRORS.md): тексты Message можно менять,
// коды нельзя. Path — JSON-путь проблемы: "steps.syntax.syntax",
// "input.emails[3]", "pipeline.foreach".
type Issue struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Step     string   `json:"step,omitempty"`
	Port     string   `json:"port,omitempty"`
	Path     string   `json:"path,omitempty"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`
	Fix      *Fix     `json:"fix,omitempty"`
}

// IsError — удобный фильтр.
func (i Issue) IsError() bool { return i.Severity == SeverityError }

// SplitIssues — совместимость со старым API (errs, warns []string):
// GUI-редактор и старые тесты продолжают работать на строках.
func SplitIssues(issues []Issue) (errs, warns []string) {
	for _, is := range issues {
		if is.Severity == SeverityError {
			errs = append(errs, is.Message)
		} else {
			warns = append(warns, is.Message)
		}
	}
	return errs, warns
}

// FilterCode — все проблемы с данным кодом (для тестов TestIssueCodes).
func FilterCode(issues []Issue, code string) []Issue {
	var out []Issue
	for _, is := range issues {
		if is.Code == code {
			out = append(out, is)
		}
	}
	return out
}

// Коды ошибок валидации — публичный контракт (protocol/v0.2/ERRORS.md).
// Тексты Message менять можно, коды — нельзя.
const (
	E_FORMAT_VERSION          = "E_FORMAT_VERSION" // warn for future versions, kept E_ for compat
	E_CYCLE                   = "E_CYCLE"
	E_FOREACH_PATH            = "E_FOREACH_PATH"
	E_FOREACH_NOT_FOUND       = "E_FOREACH_NOT_FOUND"
	E_FOREACH_SHAPE           = "E_FOREACH_SHAPE"
	E_STEP_ID_EMPTY           = "E_STEP_ID_EMPTY"
	E_STEP_ID_DUP             = "E_STEP_ID_DUP"
	E_WHEN_OP                 = "E_WHEN_OP"
	E_WHEN_PATH               = "E_WHEN_PATH"
	E_WHEN_FORWARD_REF        = "E_WHEN_FORWARD_REF"
	E_STEP_FOREACH_PATH       = "E_STEP_FOREACH_PATH"
	E_STEP_FOREACH_SHAPE      = "E_STEP_FOREACH_SHAPE"
	E_STEP_FOREACH_NOT_FOUND  = "E_STEP_FOREACH_NOT_FOUND"
	E_FOREACH_COMBO           = "E_FOREACH_COMBO"
	E_FOREACH_ITEM_NAME       = "E_FOREACH_ITEM_NAME"
	E_FOREACH_LIMIT           = "E_FOREACH_LIMIT"
	E_PARALLEL_LIMIT          = "E_PARALLEL_LIMIT"
	E_RETRY_LIMIT             = "E_RETRY_LIMIT"
	E_TIMEOUT_LIMIT           = "E_TIMEOUT_LIMIT"
	E_GATE_FOREACH            = "E_GATE_FOREACH"
	E_GATE_PARALLEL           = "E_GATE_PARALLEL"
	E_GATE_BIND               = "E_GATE_BIND"
	E_GATE_ACTIONS            = "E_GATE_ACTIONS"
	E_ON_ERROR                = "E_ON_ERROR"
	E_RETRY_ATTEMPTS          = "E_RETRY_ATTEMPTS"
	E_ON_REJECT               = "E_ON_REJECT"
	E_PLUGIN_LOAD             = "E_PLUGIN_LOAD"
	E_BIND_UNKNOWN_PORT       = "E_BIND_UNKNOWN_PORT"
	E_NETWORK_DENIED          = "E_NETWORK_DENIED"
	E_NETWORK_NOT_ENFORCEABLE = "E_NETWORK_NOT_ENFORCEABLE"
	E_NETWORK_VALUE           = "E_NETWORK_VALUE"
	E_PORT_UNBOUND            = "E_PORT_UNBOUND"
	E_PORT_SOURCE             = "E_PORT_SOURCE"
	E_TYPE_MISMATCH           = "E_TYPE_MISMATCH"
	E_FORMAT_INPUT            = "E_FORMAT_INPUT"
	E_FORMAT_MISMATCH         = "E_FORMAT_MISMATCH"
	E_OPTIONAL_REQUIRED       = "E_OPTIONAL_REQUIRED"
	E_PARALLEL_SPLIT          = "E_PARALLEL_SPLIT"
	E_FILE_REF_NOT_FOUND      = "E_FILE_REF_NOT_FOUND"
	// E_MANIFEST_* больше не объявляются. Долгое время их обещал глоб в
	// ERRORS.md, но эмитились они нулём раз: валидатор отдаёт все проблемы
	// манифеста одним E_PLUGIN_LOAD.
	//
	// Реализовывать семь кодов было бы вредно, а не полезно. Единственная
	// точка эмиссии проблем манифеста — путь валидации ПАЙПЛАЙНА
	// (validate_issues.go), поэтому код пришёл бы агенту с path вида
	// pipeline.steps.s.plugin, то есть указал бы на файл, который править не
	// нужно. И появился бы только для плагинов, уже вставленных в пайплайн,
	// при том что половина реальных проблем манифеста (id, permissions.*,
	// sandbox, secrets, network, requires.lock, port.from) в семь кодов всё
	// равно не попадает. Смесь из семи точных и одного глупого кода на один
	// класс «битый plugin.yaml» хуже одного честного.
	E_APPROVAL_VALUE = "E_APPROVAL_VALUE"
	E_GATES_VALUE    = "E_GATES_VALUE"
	// v0.35 schedule
	E_SCHEDULE_CRON       = "E_SCHEDULE_CRON"
	E_SCHEDULE_TIMEZONE   = "E_SCHEDULE_TIMEZONE"
	E_SCHEDULE_OVERLAP    = "E_SCHEDULE_OVERLAP"
	E_SCHEDULE_HUMAN_GATE = "E_SCHEDULE_HUMAN_GATE"

	W_FORMAT_VERSION_MISSING = "W_FORMAT_VERSION_MISSING"
	W_SECRETS_MISSING_ENV    = "W_SECRETS_MISSING_ENV"
	W_FOREACH_ITEM_TYPE      = "W_FOREACH_ITEM_TYPE"
	W_FOREACH_ITEM_FORMAT    = "W_FOREACH_ITEM_FORMAT"
	W_GATE_FORM_COLLISION    = "W_GATE_FORM_COLLISION"
	W_GATE_FORM_MISSING      = "W_GATE_FORM_MISSING"
	W_GATE_FORM_SKIP         = "W_GATE_FORM_SKIP"
	W_NETWORK_DECLARED       = "W_NETWORK_DECLARED"
	// W_FILESYSTEM_HOST_PATH — в bind передан путь хоста (абсолютный или с выходом
	// за каталог), а плагин объявил filesystem не readwrite и, скорее всего,
	// отклонит его на запуске. Предупреждение: ошибкой это становится только в
	// рантайме, но валидатор знает и значение, и манифест плагина.
	W_FILESYSTEM_HOST_PATH = "W_FILESYSTEM_HOST_PATH"
	// E_BIND_SOURCE_INVALID — bind объявлен, но его значение не является
	// ссылкой на контекст (input.*/steps.*). Раньше такой источник молча не
	// резолвился: для optional-порта движок его пропускал, плагин брал значение
	// по умолчанию, валидация проходила, ран завершался "done" — то есть пайплайн
	// тихо делал не то, что задано в YAML.
	E_BIND_SOURCE_INVALID   = "E_BIND_SOURCE_INVALID"
	W_PORT_OPTIONAL_UNBOUND = "W_PORT_OPTIONAL_UNBOUND"
	W_PORT_OPTIONAL_SOURCE  = "W_PORT_OPTIONAL_SOURCE"
	// v0.34: значение входа задано, но пустое. Связь при этом есть, и пайплайн
	// проходит валидацию так же чисто, как заполненный, — а ран падает уже в
	// плагине (maigret: empty_username).
	W_PORT_EMPTY_SOURCE  = "W_PORT_EMPTY_SOURCE"
	W_SECRETS_UNUSED     = "W_SECRETS_UNUSED"
	W_SECRETS_UNDECLARED = "W_SECRETS_UNDECLARED"
	W_PARALLEL_SINGLE    = "W_PARALLEL_SINGLE"
	W_FILE_REF_ROOT      = "W_FILE_REF_ROOT"
)

// Коды рантайма и API. Не Issue — они не попадают в issues[] валидации,
// но принадлежат тому же публичному контракту, поэтому объявлены здесь, а не
// строкой в месте использования.
//
// Раньше каждый из них был строковым литералом. Это значило две вещи: опечатку
// не ловил ничто, и шаг `wedra check --only=errcodes` их не видел — сверка
// смотрела только на константы. Проверка codesUnknownLiteral в internal/errdoc
// теперь требует, чтобы литерал совпадал с объявленной константой либо с
// кодом из контракта, так что опечатка падает в CI.
const (
	// MCP: ссылка на плагин вне корней --plugins/--workdir.
	E_PLUGIN_OUTSIDE_ROOT = "E_PLUGIN_OUTSIDE_ROOT"
	// MCP: file_ref вне --workdir.
	E_FILE_REF_OUTSIDE_ROOT = "E_FILE_REF_OUTSIDE_ROOT"
	// MCP: file_ref не удалось проверить, шаг не исполняется.
	E_FILE_REF_UNCHECKED = "E_FILE_REF_UNCHECKED"
	// MCP: в пайплайне гейт, а консоли человека нет — отказ до старта.
	E_NO_HUMAN_CHANNEL = "E_NO_HUMAN_CHANNEL"
	// MCP run_pipeline: первый опасный шаг (сеть/диск/секреты по capabilities
	// плагина) идёт без human_gate перед собой — отказ до старта. Раньше кода
	// не было: гейт был необязателен, и пайплайн без него исполнялся целиком.
	E_GATE_REQUIRED = "E_GATE_REQUIRED"
	// Рантайм: гейт в режиме без UI.
	E_NO_GATE_UI = "E_NO_GATE_UI"
	// Уже идёт ран (один за раз), MCP и HTTP 409.
	E_RUN_BUSY = "E_RUN_BUSY"
	// Отмена уже завершённого рана.
	E_RUN_DONE = "E_RUN_DONE"
	// HTTP 401: запрос к /api/* без cookie сессии человека (в т.ч. чтение).
	E_SESSION_REQUIRED = "E_SESSION_REQUIRED"
	// HTTP 401/429: неверный, истёкший или уже использованный одноразовый код входа.
	E_SESSION_CODE_INVALID = "E_SESSION_CODE_INVALID"
	// HTTP 403: Host запроса не в allow-list сервера (например DNS-rebinding).
	E_HOST_NOT_ALLOWED = "E_HOST_NOT_ALLOWED"
	// MCP exec_plugin: запуск без --allow-agent-exec.
	E_AGENT_EXEC_DENIED = "E_AGENT_EXEC_DENIED"
	// MCP exec_plugin: плагин агента при политике trusted.
	E_AGENT_PLUGIN_UNTRUSTED = "E_AGENT_PLUGIN_UNTRUSTED"
	// MCP exec_plugin: одновременных запусков уже 4.
	E_AGENT_EXEC_BUSY = "E_AGENT_EXEC_BUSY"
	// MCP exec_plugin: не удалось записать строку аудита.
	E_AGENT_EXEC_AUDIT = "E_AGENT_EXEC_AUDIT"
	// MCP exec_plugin: плагин объявил права, а инструмент идёт мимо
	// human_gate. Такой плагин запускается только через run_pipeline
	// с core/human_gate перед ним.
	E_AGENT_EXEC_CAPABILITIES = "E_AGENT_EXEC_CAPABILITIES"
)
