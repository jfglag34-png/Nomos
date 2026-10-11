package pipeline

import (
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration — обёртка над time.Duration для YAML-строк вида "10s", "250ms".
type Duration struct{ time.Duration }

// MarshalYAML обязателен парно к UnmarshalYAML. Без него yaml.v3 пишет
// `timeout: {duration: 0s}`, а чтение падает с «cannot unmarshal !!map into
// string». Это ловил `wedra pipeline install`: он нормализует пресет через
// yaml.Marshal и единственное место, где core-структура уходит на диск.
func (d Duration) MarshalYAML() (interface{}, error) {
	return d.Duration.String(), nil
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		d.Duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

type PipelineFile struct {
	FormatVersion string   `yaml:"format_version"`
	Pipeline      Pipeline `yaml:"pipeline"`
}

type Pipeline struct {
	Name        string                 `yaml:"name"`
	Input       map[string]interface{} `yaml:"input"`
	Foreach     string                 `yaml:"foreach"`
	ForeachItem string                 `yaml:"foreach_item"`
	ItemType    string                 `yaml:"item_type"`
	ItemFormat  string                 `yaml:"item_format"`
	// v0.16: имена переменных окружения, которые обязан задать пользователь
	// (API-ключи и т.п.). Раннер проверяет наличие до старта; сами значения
	// в YAML не живут — только имена.
	Secrets []string `yaml:"secrets"`
	// v0.17: network — "deny" запрещает шаги, чей плагин в манифесте
	// заявил сеть (declare-now: декларирование — контракт, аудит — журнал).
	Network string `yaml:"network"`
	// v0.9: gates — политика гейтов пайплайна: "human_only" запрещает
	// авто-аппрув (--yes) для всех human_gate; "any" / "" — по шагу.
	Gates string `yaml:"gates"`
	Steps []Step `yaml:"steps"`
}

type Step struct {
	ID      string            `yaml:"id"`
	Plugin  string            `yaml:"plugin"`
	OnError string            `yaml:"on_error"`
	Retry   *Retry            `yaml:"retry"`
	Timeout Duration          `yaml:"timeout"`
	Bind    map[string]string `yaml:"bind"`
	Pos     [2]int            `yaml:"pos,omitempty"`

	// v0.12: after_foreach — шаг выполняется один раз после foreach, а не per-item
	AfterForeach bool `yaml:"after_foreach"`

	// v0.20: управляющий поток на уровне шага
	// When — условие: шаг выполняется, только если оно истинно (иначе skipped)
	When When `yaml:"when,omitempty"`
	// Foreach — шаг выполняется по каждому элементу массива из пути
	// (input.* или steps.<id>.<field>). Результаты агрегируются в
	// steps.<id>_all; steps.<id> — выход последней итерации.
	Foreach     string `yaml:"foreach"`
	ForeachItem string `yaml:"foreach_item"`
	// ParallelGroup — шаги с одинаковым именем группы выполняются
	// параллельно, барьер ждёт всех веток до следующего шага.
	ParallelGroup string `yaml:"parallel_group"`

	// Loop — динамический цикл: шаги выполняются до тех пор, пока условие
	// истинно, с лимитом итераций. Condition — путь в контексте (например,
	// "steps.parser.found"), MaxIterations — потолок (по умолчанию 100).
	Loop          string `yaml:"loop"`
	LoopCondition string `yaml:"loop_condition"`
	MaxIterations int    `yaml:"max_iterations"`

	// core/human_gate
	Form     []FormField `yaml:"form"`
	Actions  []string    `yaml:"actions"`
	OnReject string      `yaml:"on_reject"`
	// v0.9: approval — "human" запрещает авто-аппрув этого гейта (--yes
	// ждёт человека); "any" / "" — как раньше.
	Approval string `yaml:"approval"`
}

// RequiresHuman — гейт этого шага нельзя одобрить автоматически (--yes).
func (p *Pipeline) RequiresHuman(st *Step) bool {
	return p.Gates == "human_only" || st.Approval == "human"
}

type Retry struct {
	Attempts int      `yaml:"attempts"`
	Delay    Duration `yaml:"delay"`
	Backoff  string   `yaml:"backoff"`
}

type FormField struct {
	Field    string `yaml:"field"`
	Editable bool   `yaml:"editable"`
	Type     string `yaml:"type"`
	Format   string `yaml:"format"`
}

// Manifest — контракт плагина
type Port struct {
	// json-теги обязательны: без них /api/plugins отдавал порты как
	// {"Type":…,"Format":…}, потому что Go сериализует поля под своими
	// именами. Остальной манифест приходит в нижнем регистре (id, version,
	// input, output), и любая проверка «d.type» в редакторе молча читала
	// undefined — то есть подсказки считали, что тип не объявлен ни у кого.
	From     string `yaml:"from" json:"from"`
	Type     string `yaml:"type" json:"type"`
	Format   string `yaml:"format" json:"format"`
	Optional bool   `yaml:"optional" json:"optional"`
}

type Runtime struct {
	Type     string   `yaml:"type" json:"type"`
	Entry    string   `yaml:"entry" json:"entry"`
	Requires []string `yaml:"requires" json:"requires"`
}

type NetworkPermission struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	AnyHost bool   `yaml:"any_host"`
	Note    string `yaml:"note"`
}

type Permissions struct {
	Network    []NetworkPermission `yaml:"network"`
	Filesystem string              `yaml:"filesystem"`
	Secrets    []string            `yaml:"secrets"`
}

type Manifest struct {
	ID          string          `yaml:"id"`
	Version     string          `yaml:"version"`
	PlatformAPI string          `yaml:"platform_api"`
	Description string          `yaml:"description"`
	Author      string          `yaml:"author"`
	Runtime     Runtime         `yaml:"runtime"`
	Input       map[string]Port `yaml:"input"`
	Output      map[string]Port `yaml:"output"`
	Permissions Permissions     `yaml:"permissions"`
	// Sandbox — МОЖЕТ ТОЛЬКО ПОНИЗИТЬ доверие, никогда не повысить.
	// "untrusted" — автор считает код внешним: плагин уходит в изолятор даже
	// если его хэш внесён в allow-list оператора. "trusted" НЕ даёт доверия
	// вообще: доверие выдаёт ядро по хэшу содержимого каталога
	// (internal/plugin.DecideTrust).
	//
	// Исторически это поле было ЕДИНСТВЕННЫМ источником доверия, и отсутствие
	// строки означало «доверен» — то есть вредоносный плагин получал права
	// пользователя, просто не написав строку. Поле оставлено как инструмент
	// понижения, а решение перешло к ядру.
	Sandbox string `yaml:"sandbox"`

	Dir string `yaml:"-"`
}

// Untrusted — плагин объявил себя внешним кодом (sandbox: untrusted).
//
// Это НЕ признак «недоверенности» вообще, а только понижение: плагин вне
// allow-list недоверен и при пустом поле. Итоговое решение — DecideTrust в
// internal/plugin, и именно оно смотрит и на это поле, и на allow-list.
func (m *Manifest) Untrusted() bool { return m != nil && m.Sandbox == SandboxUntrusted }

const (
	SandboxTrusted   = "trusted"
	SandboxUntrusted = "untrusted"
)

const (
	PlatformAPI           = "0.1"
	MaxForeachItems       = 10000
	MaxParallelWidth      = 32
	MaxRetryAttempts      = 10
	MaxRetryDelay         = 5 * time.Minute
	MaxStepTimeout        = 30 * time.Minute
	MaxAggregateItems     = 100000
	MaxLoopIterations     = 100
	MaxTotalLoopBudget    = 1000
	MaxConcurrentBranches = 32
	MaxLoopJournalEvents  = 10000
)

// NetworkHosts — человекочитаемый список заявленной сети плагина ("host:port, ...").
func NetworkHosts(m *Manifest) string {
	list := NetworkHostList(m)
	if len(list) == 0 {
		return ""
	}
	return strings.Join(list, ", ")
}

// NetworkHostList — заявленная сеть как список host:port (any_host → "*").
func NetworkHostList(m *Manifest) []string {
	if len(m.Permissions.Network) == 0 {
		return nil
	}
	parts := make([]string, 0, len(m.Permissions.Network))
	for _, np := range m.Permissions.Network {
		host := np.Host
		if np.AnyHost {
			host = "*"
		}
		if np.Port > 0 {
			parts = append(parts, host+":"+strconv.Itoa(np.Port))
		} else {
			parts = append(parts, host)
		}
	}
	return parts
}

// NetworkRequestsAnyHost — плагин явно попросил неограниченный egress.
//
// Только этот случай песочница умеет исполнить: точечный фильтр по host:port
// не реализован ни на одной платформе. Плагин со списком конкретных хостов
// различает два намерения — «мне нужен api.telegram.org:443» и «мне нужен
// интернет» — и второе обязано быть написано как any_host: true. Различие
// обязано быть явным, иначе список хостов читается как ограничение, а на
// деле им не является.
func NetworkRequestsAnyHost(m *Manifest) bool {
	if m == nil {
		return false
	}
	for _, np := range m.Permissions.Network {
		if np.AnyHost {
			return true
		}
	}
	return false
}

// NetworkHasStructuredDeclarations — плагин объявил сеть по host:port, без
// any_host. Такое объявление неисполнимо и не должно молча превращаться в
// полный доступ: до появления фильтра такой плагин получает сеть только
// явным any_host.
func NetworkHasStructuredDeclarations(m *Manifest) bool {
	if m == nil || len(m.Permissions.Network) == 0 {
		return false
	}
	return !NetworkRequestsAnyHost(m)
}

// NetworkDeny, NetworkAllow — значения поля pipeline.network.
const (
	NetworkDeny  = "deny"
	NetworkAllow = "allow"
)

// EffectiveNetwork — фактическая сетевая политика пайплайна.
//
// Пустое поле — это не «сеть разрешена», а отсутствие решения, и
// документированный дефолт равен deny. Поле не задано → deny.
//
// Раньше каждая точка кода решала это самостоятельно сравнением
// `p.Network == "deny"`, и unset-ветка нигде не совпадала с дефолтом из
// документации: гейт не срабатывал вовсе, а плагин получал
// WEDRA_NETWORK=allow. Одна функция вместо трёх независимых проверок —
// чтобы валидация и рантайм не разошлись снова.
func EffectiveNetwork(p *Pipeline) string {
	if p == nil || p.Network == "" {
		return NetworkDeny
	}
	return p.Network
}

// networkDenyBecause — чем объяснить отказ, когда сеть запрещена. Формулировка
// «пайплайн запрещает (network: deny)» при unset-поле вводит в заблуждение:
// пользователь ничего не писал, и ему показывают несуществующий запрет.
func networkDenyBecause(raw string) string {
	if raw == "" {
		return "поле network не задано, а пустое поле означает deny — укажите network: allow, чтобы разрешить"
	}
	return "пайплайн запрещает (network: deny)"
}

// portSource — источник данных порта: bind шага приоритетнее дефолтного from
func PortSource(portName string, port Port, st *Step) string {
	if st != nil && st.Bind != nil {
		if p, ok := st.Bind[portName]; ok && p != "" {
			return p
		}
	}
	return port.From
}
