package pipeline

import (
	"strings"

	"github.com/wykserdex/wedra/internal/common"
)

// Требование одобрения человека перед опасным шагом.
//
// Проблема, из-за которой файл существует. Раньше MCP проверял только одно:
// «если в пайплайне есть human_gate, нужен ли канал к человеку». Обратное —
// «а если human_gate нет вовсе» — не проверялось, и агент мог отправить
// run_pipeline с пайплайном, где ни одного гейта нет. Ран исполнялся целиком,
// и ничего в нём не происходило без ведома человека. Формулировка «у агента нет
// кнопки approve» оказывалась верной только по счастливой случайности: гейт
// можно было просто не включить.
//
// Правило здесь одно и намеренно узкое: ПЕРВЫЙ шаг, который что-то опасное
// делает, обязан идти после human_gate. Опасность определяется по объявленным
// capabilities плагина (permissions манифеста) — это единственный источник
// правды о шаге, доступный до запуска.
//
// Чего правило НЕ делает (границы, а не мелочи):
//
//   - Оно не измеряет поведение кода. permissions — декларация, а не песочница:
//     плагин, заявивший filesystem: none, всё равно может писать куда угодно
//     правами пользователя. Экран ловит заявленное намерение, а не фактическое
//     действие. Против плагина, который врёт в манифесте, работает другое —
//     sandbox: untrusted и изолятор ОС (см. SECURITY.md).
//   - Оно не оценивает when/foreach: шаг за `when` может и не выполниться, но
//     статически это не известно, поэтому шаг считается выполняющимся. Лишний
//     гейт (он не понадобился) безопаснее пропущенного.

// Capabilities — объявленные права шага: то, что он может сделать с миром.
type Capabilities struct {
	// Network — плагин объявил permissions.network (выход в сеть).
	Network bool
	// DiskWrite — плагин объявил filesystem, дающий право записи.
	DiskWrite bool
	// Secrets — плагин объявил permissions.secrets и получает их в env.
	Secrets bool
	// Unknown — манифест не прочитан, поэтому права неизвестны. Считается
	// опасным: незнание прав — не доказательство их отсутствия.
	Unknown bool
}

// Dangerous — требует ли такой набор прав одобрения человека.
func (c Capabilities) Dangerous() bool {
	return c.Network || c.DiskWrite || c.Secrets || c.Unknown
}

// Why — человекочитаемый список прав одной строкой. Пусто, только если прав нет.
func (c Capabilities) Why() string {
	var parts []string
	if c.Network {
		parts = append(parts, "сеть")
	}
	if c.DiskWrite {
		parts = append(parts, "запись на диск")
	}
	if c.Secrets {
		parts = append(parts, "чтение секретов")
	}
	if c.Unknown {
		parts = append(parts, "неизвестно: манифест не прочитан")
	}
	return strings.Join(parts, ", ")
}

// Значения permissions.filesystem, которыми плагин получает право ПИСАТЬ.
// `read`, `none` и незаданное поле таким правом не обладают.
const (
	fsWorkspace = "workspace"
	fsWrite     = "write"
	fsReadWrite = "readwrite"
)

// FilesystemWrites — объявляет ли filesystem право записи.
//
// `workspace` включён рядом с write/readwrite не по симметрии, а по смыслу:
// объявление означает «доступ к рабочей папке», и собственный фикстурный
// плагин retry_flaky пишет `_counter` рядом с собой именно с
// `filesystem: workspace`. Считать это «чтением» значило бы приписать
// манифесту то, чего в нём нет.
func FilesystemWrites(fs string) bool {
	switch fs {
	case fsWorkspace, fsWrite, fsReadWrite:
		return true
	}
	return false
}

// StepCapabilities — объявленные права шага по манифесту его плагина.
// Встроенные модули (core/*) — доверенный код в процессе ядра: прав у них нет
// по построению, и пустой набор для них — правда, а не поблажка.
func StepCapabilities(st *Step, m *Manifest) Capabilities {
	if st == nil || m == nil {
		return Capabilities{Unknown: true}
	}
	if IsBuiltin(st.Plugin) {
		return Capabilities{}
	}
	return ManifestCapabilities(m)
}

// ManifestCapabilities — объявленные права по одному манифесту плагина.
//
// Существует отдельно от StepCapabilities, потому что то же самое нужно там,
// где шага нет вовсе: инструмент MCP exec_plugin исполняет плагин напрямую,
// минуя пайплайн. Определение «что опасно» должно быть ОДНО: если каждая
// проверка объявит своё, они разъедутся, и разъедутся тихо — одна из них
// начнёт считать безопасным то, что другая считает опасным.
func ManifestCapabilities(m *Manifest) Capabilities {
	if m == nil {
		return Capabilities{Unknown: true}
	}
	return Capabilities{
		Network:   len(m.Permissions.Network) > 0,
		DiskWrite: FilesystemWrites(m.Permissions.Filesystem),
		Secrets:   len(m.Permissions.Secrets) > 0,
	}
}

// GateRequirement — вердикт проверки «нужно ли одобрение человека».
type GateRequirement struct {
	// Required — первый опасный шаг не защищён гейтом.
	Required bool
	// Step, Plugin — опасный шаг, из-за которого понадобилось одобрение.
	Step   string
	Plugin string
	// Why — права этого шага одной строкой (для сообщения агенту).
	Why string
	// GateID — id гейта, если он в пайплайне есть, но стоит ПОСЛЕ опасного
	// шага. Такой гейт не защищает: человек увидит результат, а не намерение.
	GateID string
	// GateAfter — GateID стоит после опасного шага (а не до и не вовсе).
	GateAfter bool
	// GateIneffective — причина, по которой гейт ПЕРЕД опасным шагом не
	// считается одобрением (GateID — этот гейт). Пусто, если такого гейта нет.
	// Гейт, который может не выполниться или чей отказ не останавливает ран,
	// одобрения человека не гарантирует.
	GateIneffective string
}

// IsHumanGate — встроенный модуль ИМЕННО гейт человека.
//
// Отдельно от IsBuiltin, который истинен и для core/text_stats: text_stats —
// не гейт. Считать его гейтом значило бы признать опасный пайплайн
// одобренным из-за шага, который ничего не одобряет.
func IsHumanGate(ref string) bool {
	canonical, ok := common.CanonicalBuiltinRef(ref)
	return ok && canonical == common.HumanGatePluginRef
}

// whenSet — у шага задано условие when.
func whenSet(w When) bool {
	return w.Path != "" || w.Op != "" || w.Value != nil
}

// GateIneffectiveReason — почему гейт g НЕ гарантирует одобрение человека для
// шага guarded. Пустая строка — гейт действует.
//
// Позиция гейта в списке необходима, но недостаточна. Гейт стоит перед опасным
// шагом и при этом одобрения не даёт, если:
//
//   - у него задан when: условие может оказаться ложным, гейт будет пропущен
//     (step_skipped), а опасный шаг выполнится без человека. Статически это
//     неизвестно, поэтому гейт с when не засчитывается;
//   - on_error: skip — ошибка гейта (например, закрытие канала) его пропускает;
//   - on_reject: continue — человек нажал reject, а ран идёт дальше: отказ
//     превращается в формальность;
//   - after_foreach при foreach-пайплайне, когда охраняемый шаг идёт по
//     элементам: шаги по элементам выполняются до шагов after_foreach,
//     то есть гейт отработает ПОСЛЕ опасного шага.
//
// Направление ошибки то же, что у всего правила: лишний отказ стоит агенту
// одной правки YAML, пропущенный гейт — исполнения без человека.
func GateIneffectiveReason(pf *PipelineFile, g, guarded *Step) string {
	if g == nil {
		return "гейт не найден"
	}
	if whenSet(g.When) {
		return "у гейта задано when: он может быть пропущен, а опасный шаг выполнится без человека"
	}
	if g.OnError == "skip" {
		return "у гейта on_error: skip: ошибка гейта пропускает его"
	}
	if g.OnReject == "continue" {
		return "у гейта on_reject: continue: отказ человека не останавливает ран (нужен stop или значение по умолчанию)"
	}
	if pf != nil && guarded != nil && pf.Pipeline.Foreach != "" && g.AfterForeach && !guarded.AfterForeach {
		return "гейт с after_foreach выполняется после шагов по элементам, то есть после опасного шага"
	}
	return ""
}

// EvaluateGateApproval — нужен ли гейт перед опасными шагами пайплайна.
//
// Каждый опасный шаг проверяется отдельно: перед ним должен стоять хотя бы
// один ДЕЙСТВУЮЩИЙ гейт (GateIneffectiveReason == ""). Вердикт возвращается по
// первому шагу, не прошедшему проверку. Если опасных шагов нет, одобрение не
// требуется — иначе правило было бы слишком широким (агент не смог бы
// запустить, например, подсчёт слов).
func EvaluateGateApproval(pf *PipelineFile, eng Engine) GateRequirement {
	if pf == nil {
		return GateRequirement{}
	}
	steps := pf.Pipeline.Steps
	for i := range steps {
		st := &steps[i]
		if IsHumanGate(st.Plugin) {
			continue
		}
		caps := stepCapsOrUnknown(st, eng)
		if !caps.Dangerous() {
			continue
		}
		var badGate *Step
		badWhy := ""
		approved := false
		for j := 0; j < i; j++ {
			g := &steps[j]
			if !IsHumanGate(g.Plugin) {
				continue
			}
			why := GateIneffectiveReason(pf, g, st)
			if why == "" {
				approved = true
				break
			}
			if badGate == nil {
				badGate, badWhy = g, why
			}
		}
		if approved {
			continue
		}
		req := GateRequirement{Required: true, Step: st.ID, Plugin: st.Plugin, Why: caps.Why()}
		switch {
		case badGate != nil:
			req.GateID, req.GateIneffective = badGate.ID, badWhy
		default:
			// Гейта выше нет. Если он есть ниже, скажем об этом прямо.
			for j := i + 1; j < len(steps); j++ {
				if IsHumanGate(steps[j].Plugin) {
					req.GateID, req.GateAfter = steps[j].ID, true
					break
				}
			}
		}
		return req
	}
	return GateRequirement{}
}

// stepCapsOrUnknown — права шага, а при нечитаемом манифесте — «неизвестно».
// Отказ по умолчанию: не удалось доказать, что шаг безобиден.
func stepCapsOrUnknown(st *Step, eng Engine) Capabilities {
	if IsBuiltin(st.Plugin) {
		return Capabilities{}
	}
	if eng == nil {
		return Capabilities{Unknown: true}
	}
	m, err := eng.LoadManifest(st.Plugin)
	if err != nil {
		return Capabilities{Unknown: true}
	}
	return StepCapabilities(st, m)
}
