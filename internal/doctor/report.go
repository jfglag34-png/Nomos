package doctor

import (
	"fmt"
	"strings"

	"github.com/wykserdex/wedra/internal/plugin"
)

// Вид донора. Не коды протокола (см. константы статусов), а различение для
// вывода: донор, совпадающий с пакетом из runtime.requires, чинится иначе, чем
// чужой CLI.
//
// Последние два Run не выдаёт — они остались от прежней классификации, где у
// каждого случая был свой вид вместо нынешних булевых полей
// (DonorInstalledNoScript / DonorUnknown). Удалять не стали: словарь видов
// донора читается вместе с полем DonorKind, а сокращать его молча — значит
// менять объём правки шире переноса.
const (
	DonorKindExternal        = "external"
	DonorKindSameAsRequires  = "same_as_requires"
	DonorKindNotInstalled    = "not_installed"
	DonorKindScriptNotInPath = "installed_script_not_in_path"
)

// Plugin — состояние одного плагина.
//
// JSON-теги — побайтово те же, что были у doctorPlugin: вывод `wedra doctor
// --json` документирован, в README и в чужих скриптах по нему ходят. Экспорт
// типов не должен был изменить ни байт сериализации.
type Plugin struct {
	ID     string `json:"id"`
	Dir    string `json:"dir"`
	Status string `json:"status"`

	// Requires — объявленные runtime.requires (пусто у большинства).
	Requires []string `json:"requires,omitempty"`
	// MissingRequires — из них отсутствуют в интерпретаторе.
	MissingRequires []string `json:"missing_requires,omitempty"`

	// Donor — внешний инструмент, упомянутый в описании.
	Donor string `json:"donor,omitempty"`
	// DonorFound — найден ли он в PATH (пусто Donor → поле не значимо).
	DonorFound bool `json:"donor_found,omitempty"`
	// DonorFoundName — под каким именно именем нашёлся: у пакета scoutsuite
	// бинарь называется scout, и это полезно видеть.
	DonorFoundName string `json:"donor_found_name,omitempty"`
	// DonorCandidates — какие имена проверялись в PATH: имя пакета плюс
	// литералы shutil.which из кода плагина.
	DonorCandidates []string `json:"donor_candidates,omitempty"`
	// Install — готовая команда, которую можно скопировать.
	Install string `json:"install,omitempty"`

	// DonorKind — external либо same_as_requires (дефект 2).
	DonorKind string `json:"donor_kind,omitempty"`
	// DonorMissing — пакета донора нет: совет «pip install X» уместен.
	DonorMissing bool `json:"donor_missing,omitempty"`
	// DonorInstalledNoScript — пакет УСТАНОВЛЕН, но его консольного скрипта
	// нет в PATH. «pip install X» тут бесполезно и было бы ложью.
	DonorInstalledNoScript bool `json:"donor_installed_no_script,omitempty"`
	// DonorUnknown — определить надёжно не удалось; совет нейтральный.
	DonorUnknown bool `json:"donor_unknown,omitempty"`
	// DonorAbsentFromIndex — пакета донора нет на PyPI. Проба интерпретатора
	// про это молчит: она спрашивает «установлен ли пакет ЗДЕСЬ», а не «существует
	// ли он вообще». Поэтому «не установлен» долго выдавало `pip install X` для
	// имён, которых на индексе нет, — команду, которая не может сработать.
	DonorAbsentFromIndex bool `json:"donor_absent_from_index,omitempty"`
	// DonorIndexUnknown — наличие пакета на PyPI проверить не удалось (сети нет,
	// индекс недоступен). Это НЕ то же, что DonorAbsentFromIndex: «не знаю» не
	// повод ни советовать установку, ни объявлять пакет отсутствующим.
	DonorIndexUnknown bool `json:"donor_index_unknown,omitempty"`
	// DonorScripts — имена консольных скриптов пакета (entry_points).
	DonorScripts []string `json:"donor_scripts,omitempty"`

	// idxState — ответ индекса для этого донора. Не в JSON: наружу уходят
	// разобранные DonorAbsentFromIndex и DonorIndexUnknown, а сырой ответ
	// нужен только чтобы выбрать текст совета.
	idxState indexState
}

// Report — весь ответ команды.
type Report struct {
	OK bool `json:"ok"`

	Python plugin.InterpreterInfo `json:"python"`
	// PythonError — интерпретатор не найден: python-плагины не запустятся
	// ВООБЩЕ, и проверка пакетов бессмысленна.
	PythonError string `json:"python_error,omitempty"`

	PluginsDir string `json:"plugins_dir"`
	Total      int    `json:"total"`
	Ready      int    `json:"ready"`
	NeedsDep   int    `json:"needs_dependency"`
	NeedsDonor int    `json:"needs_external_tool"`

	Plugins []Plugin `json:"plugins"`
	// UnverifiedDonors — плагины, у которых донора нет в описании манифеста,
	// поэтому doctor их не проверял. Пустой список означает «все проверены».
	UnverifiedDonors []string `json:"unverified_donors,omitempty"`

	// Summary — та же строка, что печатается человеку.
	Summary string `json:"summary"`
}

// summary — одна строка человеческим языком.
func summary(rep Report) string {
	if rep.Total == 0 {
		return fmt.Sprintf("плагины не найдены в %s — проверь, что бинарь запущен из корня репозитория", rep.PluginsDir)
	}
	if rep.PythonError != "" && rep.Ready == 0 {
		return fmt.Sprintf("не нашлось ни одного готового плагина: сначала интерпретатор Python — %s", rep.PythonError)
	}
	if rep.NeedsDep == 0 && rep.NeedsDonor == 0 {
		return fmt.Sprintf("готовы все %d плагинов — окружение настроено", rep.Total)
	}
	var parts []string
	if rep.NeedsDep > 0 {
		parts = append(parts, plural(rep.NeedsDep, "ждёт pip-пакет из runtime.requires",
			"ждут pip-пакет из runtime.requires", "ждут pip-пакеты из runtime.requires"))
	}
	if rep.NeedsDonor > 0 {
		parts = append(parts, fmt.Sprintf("%s внешнего инструмента (runtime.requires его не ставит)",
			plural(rep.NeedsDonor, "ждёт", "ждут", "ждут")))
	}
	return fmt.Sprintf("готовы %d из %d плагинов, остальным нужно доустановить: %s",
		rep.Ready, rep.Total, strings.Join(parts, "; "))
}

// plural — согласование числительного с существительным. Русский текст без
// него читается как «1 ждут», и это первое, на что смотрит человек, решая
// доверять ли отчёту.
func plural(n int, one, few, many string) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return fmt.Sprintf("%d %s", n, one)
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 10 || n%100 >= 20):
		return fmt.Sprintf("%d %s", n, few)
	default:
		return fmt.Sprintf("%d %s", n, many)
	}
}
