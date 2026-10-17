package cli

import (
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/pipeline"
	"github.com/wykserdex/wedra/internal/plugin"
)

// doctor: ответ на вопрос «я скачал бинарь, ничего не настроил, что мне
// сломано и что делать».
//
// Команда только ОПРАШИВАЕТ: ни один плагин не запускается, ничего не ставится.
// Единственное, что выполняется, — интерпретатор Python: без его запуска не
// узнать ни версию, ни какие пакеты уже стоят, а именно это и есть половина
// ответа.
//
// Сеть: используется ровно для одной вещи — GET к индексу PyPI по имени
// донора, который не найден локально. Это чтение одного JSON, не установка и не
// запуск кода. Причина не косметическая: проба интерпретатора отвечает «установлен
// ли пакет здесь», а не «существует ли он на PyPI», поэтому без этой проверки
// doctor выдавал `pip install X` для имён, которых на индексе нет, — то есть
// совет, который невозможно выполнить. Если сети нет, doctor говорит «не удалось
// проверить», а не притворяется, что знает. Прежняя формулировка «сеть не
// используется» была верна до этой проверки и перестала быть правдой.
//
// Три состояния плагина:
//
//	ready               — всё, что нужно, на месте
//	needs_dependency    — не хватает пакета из runtime.requires
//	needs_external_tool — нужен донор, которого нет в PATH
//
// Последнее — отдельное состояние, а не подвид первого, потому что
// runtime.requires его НЕ ставит и не может: `pip install arjun` ставит
// одноимённый пакет, а `joomscan` — это Perl-скрипт, `webanalyze` — Go-
// программа, и часть доноров ставится только из git. Человек видит в
// манифесте «pip install X», пробует, и либо получает ошибку, либо (для
// git/Perl/Go) не получает вообще ничего.

// Статусы плагина. Это НЕ коды ошибок протокола: они не попадают ни в журнал
// рана, ни в issues валидации, и намеренно не имеют вида E_*/W_*, чтобы гейт
// errcodes (коды в Go ↔ protocol/v0.2/ERRORS.md) не считал их контрактом.
const (
	doctorReady    = "ready"
	doctorNeedsDep = "needs_dependency"
	doctorNeedsDon = "needs_external_tool"
)

// Вид донора. Не коды протокола (см. выше), а различение для вывода: донор,
// совпадающий с пакетом из runtime.requires, чинится иначе, чем чужой CLI.
const (
	doctorDonorExternal        = "external"
	doctorDonorSameAsRequires  = "same_as_requires"
	doctorRequiresNotInstalled = "not_installed"
	doctorRequiresScriptNotIn  = "installed_script_not_in_path"
)

// pipInstallRe — «pip install X» из описания манифеста. Именно отсюда человек
// берёт команду установки, поэтому и цитировать её будем оттуда же.
//
// Первая группа после `pip install` — то, что pip считает целью установки.
// Спецслучаи отсекаются: `-r requirements.txt` и `git+https://…` не называют
// пакета, и подставить вместо них имя — значит выдумать команду, которая не
// сработает (для VCS-цели pip вообще требует префикс git+, а не голое имя).
var pipInstallRe = regexp.MustCompile(`(?i)pip3?\s+install\s+([^\s]+)`)

// vcsPrefixes — цели, которые pip ставит не из индекса пакетов.
var vcsPrefixes = []string{"git+", "hg+", "svn+", "bzr+", "http://", "https://", "file://", "-e", "."}

// urlInstallTargetRe — «цель установки» с адресом: `https://…tar.gz?a=b`,
// `https://…#egg=x`, `git://…`, `git+https://…`, `file://…`.
//
// Проверяется ДО обрезания описания по запятой/двоеточию, и это единственное
// место, где правило может сработать: обрезание уничтожает `://`, после чего
// `pip install https://github.com/x/y-1.0.tar.gz?a=b` превращается в `https`
// — валидное имя пакета, то есть в донора, которого не существует.
//
// Перестановка проверки префиксов VCS в начало задачи не решает: список
// префиксов по построению неполон (`git://` в нём нет, `ssh://` тоже, а
// список пришлось бы дополнять всеми возможными схемами), и мусор, который
// обязан чиститься, от ссылки почти не отличим — `vt-py):` и `https://…`
// это одно и то же двоеточие в том же месте. Отличает их ровно `//` после
// двоеточия, поэтому здесь не список, а структурный признак: `схема://`.
var urlInstallTargetRe = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.\-]*://`)

// whichRe — литералы shutil.which("…") из кода плагина. Это самый честный
// источник: плагин сам говорит, какое имя бинаря ищет, и это может отличаться
// от имени пакета (пакет scoutsuite даёт бинарь scout, oletools — olevba).
var whichRe = regexp.MustCompile(`shutil\.which\(\s*"([^"]+)"`)

// doctorPlugin — состояние одного плагина.
type doctorPlugin struct {
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

// doctorReport — весь ответ команды.
type doctorReport struct {
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

	Plugins []doctorPlugin `json:"plugins"`
	// UnverifiedDonors — плагины, у которых донора нет в описании манифеста,
	// поэтому doctor их не проверял. Пустой список означает «все проверены».
	UnverifiedDonors []string `json:"unverified_donors,omitempty"`

	// Summary — та же строка, что печатается человеку.
	Summary string `json:"summary"`
}

// RunDoctor — wedra doctor [--json]
func RunDoctor(args []string) {
	asJSON := false
	pluginsDir := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "--plugins="):
			pluginsDir = strings.TrimPrefix(a, "--plugins=")
		case a == "--plugins" && i+1 < len(args):
			i++
			pluginsDir = args[i]
		default:
			fmt.Printf("неизвестный флаг doctor %q\n", a)
			os.Exit(2)
		}
	}

	report := runDoctor(pluginsDir)

	if asJSON {
		printJSON(report)
		if !report.OK {
			os.Exit(1)
		}
		return
	}
	printDoctorHuman(report)
	if !report.OK {
		os.Exit(1)
	}
}

func runDoctor(pluginsDir string) doctorReport {
	if pluginsDir == "" {
		pluginsDir = "plugins"
	}
	rep := doctorReport{PluginsDir: pluginsDir, OK: true}

	// 1. Интерпретатор — тем же кодом, каким реально стартуют плагины.
	info, err := plugin.PythonInfo()
	if err != nil {
		rep.PythonError = err.Error()
		rep.OK = false
	} else {
		rep.Python = info
	}

	manifests := scanDoctorManifests(pluginsDir)
	rep.Total = len(manifests)

	// 2. Пакеты: и из runtime.requires, и доноры из описаний — одним опросом
	// интерпретатора. Доноры нужны не для «стоит ли», а чтобы отличить
	// «не установлен» от «установлен, но скрипта нет в PATH» (дефект 1):
	// советы у этих случаев противоположны.
	//
	// Если интерпретатора нет, проверять нечем: python-плагины не запустятся
	// вообще, и «отсутствующий пакет» был бы ложью, маскирующей настоящую
	// причину. Поэтому в этом случае requires помечаются отсутствующими, но
	// причина называется отдельно (PythonError), а не ими.
	wanted := []string{}
	seenPkg := map[string]bool{}
	addProbe := func(name string) {
		// Пробуем и имя пакета, и имя модуля, если они различаются:
		// иначе dnspython (модуль dns) читался бы как отсутствующий.
		for _, probe := range requirementImportNames(name) {
			if seenPkg[probe] {
				continue
			}
			seenPkg[probe] = true
			wanted = append(wanted, probe)
		}
	}
	for _, m := range manifests {
		for _, r := range m.Requires {
			if name := requirementName(r); name != "" {
				addProbe(name)
			}
		}
		if donor, ok := donorFromDescription(m.Description); ok {
			addProbe(donor)
		}
	}
	pkgs := map[string]plugin.PackageInfo{}
	if rep.PythonError == "" && len(wanted) > 0 {
		if got, perr := plugin.PythonPackages(rep.Python.Path, wanted); perr == nil {
			pkgs = got
		} else {
			// Проба не отработала: честнее сказать об этом, чем объявить все
			// пакеты отсутствующими.
			rep.PythonError = perr.Error()
			rep.OK = false
		}
	}

	// 3. Внешние доноры: есть ли такой бинарь в PATH.
	plugins := make([]doctorPlugin, 0, len(manifests))
	for _, m := range manifests {
		p := doctorPlugin{
			ID:       m.ID,
			Dir:      m.Dir,
			Status:   doctorReady,
			Requires: m.Requires,
		}
		// Сведения о пакетах, которые плагин объявил в runtime.requires.
		// Ключ — имя из манифеста: к нему же потом привязывается классификация
		// донора (дефект 2), и искать его по алиасу модуля нельзя.
		reqInfo := map[string]plugin.PackageInfo{}
		for _, r := range m.Requires {
			name := requirementName(r)
			if name == "" {
				continue
			}
			if info, ok := lookupPackage(pkgs, name); ok {
				reqInfo[name] = info
			}
		}
		for _, r := range m.Requires {
			name := requirementName(r)
			if name == "" {
				continue
			}
			info, known := reqInfo[name]
			if known && (info.Importable || info.Installed) {
				continue
			}
			p.MissingRequires = append(p.MissingRequires, r)
		}
		if len(p.MissingRequires) > 0 {
			p.Status = doctorNeedsDep
			p.Install = installCommand(p.MissingRequires)
		}

		// Донор: «pip install X» из описания манифеста.
		if donor, ok := donorFromDescription(m.Description); ok {
			p.Donor = donor
			p.DonorCandidates = donorCandidates(donor, m.Dir)
			foundPath, foundName := donorInPath(p.DonorCandidates)

			// Дефект 2. Если «внешний инструмент» совпадает с пакетом из
			// runtime.requires того же плагина, внешнего инструмента тут нет:
			// это та же самая pip-зависимость, что и в requires, и совет
			// «установи инструмент» только путает. community/exifread —
			// ровно этот случай: `pip install exifread` в описании и
			// `requires: [exifread==3.5.1]` в манифесте.
			sameAsRequires := false
			for r := range m.Requires {
				if requirementName(m.Requires[r]) == donor {
					sameAsRequires = true
					break
				}
			}

			if sameAsRequires {
				p.DonorKind = doctorDonorSameAsRequires
			} else {
				p.DonorKind = doctorDonorExternal
			}

			if !foundPath {
				switch {
				case sameAsRequires:
					// Пакет из requires; статус уже выставлен выше по
					// MissingRequires. Отдельно ничего не советуем, чтобы не
					// предлагать одно и то же дважды.
					if p.Status == doctorReady {
						// Объявлен в requires, проба показала, что он есть, но
						// в PATH нет одноимённого бинаря: плагин запускает пакет
						// сам (паттерн C), поэтому бинарь ему не нужен.
						p.Status = doctorReady
					}
				default:
					info, known := lookupPackage(pkgs, donor)
					switch {
					case !known || !info.ProbeOK:
						// Установить нельзя надёжно определить — не выдумываем.
						p.DonorUnknown = true
					case !info.Installed && !info.Importable:
						// Дефект 3. «Пакета нет здесь» не равно «пакета нет на
						// индексе»: проба спрашивает только первое. Поэтому перед
						// советом об установке индекс опрашивается — и его
						// недоступность остаётся недоступностью, а не поводом
						// советовать наугад.
						p.DonorMissing = true
						p.idxState = doctorIndexLookup(donor)
						switch p.idxState {
						case indexAbsent:
							p.DonorAbsentFromIndex = true
						case indexUnknown:
							p.DonorIndexUnknown = true
						}
					default:
						// Дефект 1. Пакет стоит, а его консольного скрипта нет
						// в PATH. pip кладёт скрипты в Scripts\, который в PATH
						// может не быть — и «pip install X» тут бесполезно:
						// пакет уже установлен.
						p.DonorInstalledNoScript = true
						p.DonorScripts = info.Scripts
					}
					if p.Status == doctorReady {
						p.Status = doctorNeedsDon
					}
				}
			}
			p.DonorFound = foundPath
			if foundPath {
				p.DonorFoundName = foundName
			}
			if p.Status == doctorNeedsDon {
				switch {
				case p.DonorInstalledNoScript:
					p.Install = pathAdvice(rep, p)
				case p.DonorUnknown:
					p.Install = "проверь вручную: pip show " + donor
				case p.DonorMissing:
					p.Install = indexAdvice(donor, p.idxState)
				default:
					p.Install = "pip install " + donor
				}
			}
		}

		switch p.Status {
		case doctorReady:
			rep.Ready++
		case doctorNeedsDep:
			rep.NeedsDep++
		case doctorNeedsDon:
			rep.NeedsDonor++
		}
		plugins = append(plugins, p)
	}

	rep.UnverifiedDonors = unverifiedDonorPlugins(manifests, plugins)

	rep.Plugins = plugins
	rep.Summary = doctorSummary(rep)
	// OK — это не «ничего не сломалось», а «плагинов есть и они готовы».
	// Пустой каталог плагинов тоже неуспех: человек запустил doctor, ожидая
	// узнать про 99 плагинов, и «ну ноль проблем» было бы ложью. То же самое с
	// отсутствующим интерпретатором — плагины-то есть, запустить их нельзя.
	if rep.NeedsDep > 0 || rep.NeedsDonor > 0 || rep.PythonError != "" || rep.Total == 0 {
		rep.OK = false
	}
	return rep
}

// lookupPackage — сведения о пакете по имени ИЗ МАНИФЕСТА.
//
// Проба идёт по обоим возможным именам: имя пакета на PyPI и имя модуля могут
// различаться (dnspython ставит модуль dns). Второй аргумент — «найдено ли
// вообще», чтобы «не знаю» не путалось с «нет».
func lookupPackage(pkgs map[string]plugin.PackageInfo, name string) (plugin.PackageInfo, bool) {
	if info, ok := pkgs[name]; ok {
		return info, true
	}
	for _, alias := range requirementImportNames(name) {
		if info, ok := pkgs[alias]; ok {
			return info, true
		}
	}
	return plugin.PackageInfo{}, false
}

// indexState — ответ индекса пакетов. Три состояния, а не два, и это не
// педантизм: «нет на индексе» и «не удалось проверить» требуют от doctor
// противоположного поведения. Первое — можно сказать «ставьте из исходников».
// Второе — нельзя сказать ничего, кроме «проверь сам».
type indexState int

const (
	indexUnknown indexState = iota
	indexAbsent
	indexPresent
)

// doctorIndexLookup — проверка «есть ли такой пакет на PyPI».
//
// Именно проверка, а не установка: doctor по-прежнему ничего не ставит и
// ничего не запускает. Это HTTP GET одного JSON-индекса.
//
// Переменная, а не прямой вызов намеренно: она меняет ответ, по которому
// doctor решает, что человеку показать, а юнит-тесты doctor обязаны быть
// детерминированными и не ходить в сеть. Подменяет её только doctor_test.go.
var doctorIndexLookup = pypiIndexLookup

// pypiIndexLookup спрашивает индекс по имени пакета.
//
// Ровно GET https://pypi.org/pypi/<имя>/json: ответ 404 у pip означает
// «дистрибутива нет», и это единственный сигнал, на который здесь можно
// опереться. Код возврата pip для этого НЕ годится — проверено на этой
// машине: `pip index versions recon-ng` печатает «ERROR: No matching
// distribution found» в stderr и при этом завершается с кодом 0, то есть
// судить по коду возврата там, где pip отвечает в stderr, — значит
// выдумывать результат.
func pypiIndexLookup(name string) indexState {
	if name == "" {
		return indexUnknown
	}
	client := &http.Client{Timeout: 10 * time.Second}
	url := "https://pypi.org/pypi/" + neturl.PathEscape(name) + "/json"
	resp, err := client.Get(url)
	if err != nil {
		return indexUnknown
	}
	defer resp.Body.Close()
	// Тело не читаем целиком: нужен только код ответа, а индекс отдаёт
	// мегабайты JSON, из которых нужны две строки.
	switch {
	case resp.StatusCode == http.StatusOK:
		return indexPresent
	case resp.StatusCode == http.StatusNotFound:
		return indexAbsent
	default:
		// 500, 403, редирект на captcha и прочее — это «не знаю», а не «нет».
		return indexUnknown
	}
}

// indexAdvice — что сказать человеку, когда донора нет локально.
//
// Раньше здесь был безусловный `pip install <донор>`, и это ровно тот совет,
// который нельзя давать вслепую: для `recon-ng` и `spiderfoot` он был
// нерабочим. Теперь состояние индекса известно и отличено от «не знаю».
func indexAdvice(donor string, state indexState) string {
	switch state {
	case indexAbsent:
		return "пакета " + donor + " на PyPI нет — `pip install` не поможет; ставьте из исходников (git clone) или уберите плагин"
	case indexPresent:
		return "pip install " + donor
	default:
		return "не установлено; наличие " + donor + " на PyPI проверить не удалось — `pip install` может не сработать, сверьтесь вручную"
	}
}

// pathAdvice — что сказать, когда пакет стоит, а скрипта нет в PATH.
//
// Самое полезное здесь не «установите», а имя каталога, который надо добавить
// в PATH. pip кладёт консольные скрипты в Scripts (Scripts\ на Windows), и его
// обычно там нет: поэтому «pip install X» для уже установленного пакета не
// помогает ничего — это был ложный совет.
func pathAdvice(rep doctorReport, p doctorPlugin) string {
	var b strings.Builder
	b.WriteString("пакет уже установлен, но его скрипта нет в PATH — установка не поможет")
	if len(p.DonorScripts) > 0 {
		b.WriteString("; скрипт пакета: " + strings.Join(p.DonorScripts, ", "))
	}
	if rep.Python.ScriptsDir != "" {
		if rep.Python.ScriptsDirInPATH {
			b.WriteString("; каталог " + rep.Python.ScriptsDir + " в PATH есть — проверь имя скрипта")
		} else {
			b.WriteString("; добавь в PATH: " + rep.Python.ScriptsDir)
		}
	}
	return b.String()
}

// pipWord — «pip install» в любом регистре, как его пишут в README и в коде.
var pipWord = regexp.MustCompile(`(?i)pip3?\s+install`)

// unverifiedDonorPlugins — плагины, чей внешний инструмент doctor НЕ проверял.
//
// Проверяется только то, что названо в описании манифеста. Если донора там нет,
// но «pip install» встречается в README или в коде, такой плагин молча попал бы
// в «готовы» — а это ровно то враньё, ради которого оговорка и нужна.
func unverifiedDonorPlugins(manifests []doctorManifest, reported []doctorPlugin) []string {
	checked := map[string]bool{}
	for _, p := range reported {
		if p.Donor != "" {
			checked[p.ID] = true
		}
	}
	var out []string
	for _, m := range manifests {
		if checked[m.ID] {
			continue
		}
		if mentionsPipInstall(m.Dir) {
			out = append(out, m.ID)
		}
	}
	sort.Strings(out)
	return out
}

// mentionsPipInstall — есть ли упоминание установки в README или в коде
// плагина. Читаются только два типа файлов: README* и .py, потому что
// остальное (манифест с его описанием, requirements.lock) уже учтено выше.
func mentionsPipInstall(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == "__pycache__" || name == ".git" {
				continue
			}
			if mentionsPipInstall(filepath.Join(dir, name)) {
				return true
			}
			continue
		}
		if !strings.HasPrefix(strings.ToUpper(name), "README") && !strings.HasSuffix(name, ".py") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if pipWord.Match(raw) {
			return true
		}
	}
	return false
}

// doctorManifest — минимум из манифеста, нужный doctor'у.
type doctorManifest struct {
	ID          string
	Dir         string
	Description string
	Requires    []string
}

// scanDoctorManifests читает манифесты напрямую из каталога.
//
// Не через core.ScanPlugins(): тот идёт по относительным путям, молча пропускает
// битый манифест и ничего не говорит. Здесь битый манифест — это плагин без
// doctor-вердикта, и человек должен узнать об этом, а не решить, что плагинов
// просто меньше.
func scanDoctorManifests(dir string) []doctorManifest {
	var out []doctorManifest
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, group := range entries {
		if !group.IsDir() {
			continue
		}
		groupDir := filepath.Join(dir, group.Name())
		// Раскладка и плоская (plugins/<id>) и вложенная
		// (plugins/official/<id>, plugins/community/<id>).
		candidates := []string{groupDir}
		if sub, err := os.ReadDir(groupDir); err == nil {
			for _, e := range sub {
				if e.IsDir() {
					candidates = append(candidates, filepath.Join(groupDir, e.Name()))
				}
			}
		}
		for _, cd := range candidates {
			raw, err := os.ReadFile(filepath.Join(cd, "plugin.yaml"))
			if err != nil {
				continue
			}
			// Разбор — тем же кодом, что и у ядра (pipeline.DecodeManifest),
			// а не своя копия yaml: иначе doctor считал бы готовым плагин,
			// который ран потом не примет.
			var m pipeline.Manifest
			if err := pipeline.DecodeManifest(raw, &m); err != nil || m.ID == "" {
				continue
			}
			out = append(out, doctorManifest{
				ID:          m.ID,
				Dir:         cd,
				Description: m.Description,
				Requires:    m.Runtime.Requires,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// donorFromDescription достаёт имя пакета из «pip install X» в описании.
//
// Второй аргумент — «упомянут ли вообще». Совпадение есть всегда положительное;
// «нет» означает «в описании нечем учиться», и тогда doctor молчит, а не
// выдумывает донора.
func donorFromDescription(desc string) (string, bool) {
	m := pipInstallRe.FindStringSubmatch(desc)
	if len(m) < 2 {
		return "", false
	}
	// Регулярка берёт первый «кусок» без пробелов, а в описаниях это конец
	// фразы: «…(import vt, REST API v3, pip install vt-py): плагин идёт» даёт
	// `vt-py):`, а «`pip install webanalyze` на PyPI нет» — `webanalyze\``.
	// Такие хвосты и есть дефект 3: команда с ними не сработает.
	name := strings.TrimSpace(m[1])
	// Обратный слэш на конце — это закрывающая markdown-кавычка в
	// «`pip install webanalyze`», а не часть имени.
	name = strings.TrimRight(name, "\\")
	name = strings.Trim(name, `"'()[]{}<>`+"`")
	// Ссылка или VCS-цель пакетом не является: имени, по которому можно
	// проверить наличие в PATH, у неё нет, а подставленное вместо неё имя
	// дало бы человеку команду, которая не сработает. Именно здесь, до
	// обрезания по «,:;» — иначе `https://…` станет `https` (см. комментарий
	// к urlInstallTargetRe).
	if urlInstallTargetRe.MatchString(name) {
		return "", false
	}
	// Запятая и двоеточие могут быть и внутри: «pip install x, y».
	if i := strings.IndexAny(name, ",:;"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, ".")
	name = strings.Trim(name, `"'()[]{}<>`+"` ")
	if name == "" {
		return "", false
	}
	// Пин `package==version` — единственный «хвост», который остаётся в
	// описании уже ПОСЛЕ чистки мусора выше: «Пин: pip install pefile==2024.8.26».
	// Имя пакета тут — то, что до `==`, и разбирать это надо ДО проверки
	// validPackageName: иначе плагин с честным пином (community/pefile,
	// community/detect_secrets) объявляется «донор не проверен» и попадает в
	// unverified_donors, хотя проверять тут нечего — пакет объявлен в
	// runtime.requires и виден среди requires.
	if strings.Contains(name, "==") {
		unpinned, ok := packageNameFromPin(name)
		if !ok {
			return "", false
		}
		name = unpinned
	}
	// `-r requirements.txt` и `--upgrade X` пакетом не являются: у них нет
	// имени, по которому можно проверить наличие в PATH. Ссылка и VCS-цель
	// отсечены выше (urlInstallTargetRe) — до обрезания, где от них остаются
	// обрубки вида `https`/`git`; vcsPrefixes здесь второй рубеж, на случай
	// если схема попадёт в цель установки в нестандартном написании.
	if strings.HasPrefix(name, "-") {
		return "", false
	}
	lower := strings.ToLower(name)
	for _, p := range vcsPrefixes {
		if strings.HasPrefix(lower, p) {
			return "", false
		}
	}
	// Остатки пути — это локальный каталог, а не пакет.
	if strings.ContainsAny(name, "/\\") {
		return "", false
	}
	// Последний рубеж: имя пакета не может начинаться с цифры и не может
	// содержать символов, которых в нём не бывает. Список — из фактического
	// мусора в 99 манифестах, а не из догадок.
	if !validPackageName(name) {
		return "", false
	}
	return name, true
}

// validPackageName — похоже ли это на имя пакета PyPI.
//
// Верхняя граница длины и запрет пробелов взяты из правил PyPI: длинное или с
// пробелом имя заведомо не пакет, и подставлять его в команду установки — значит
// выдумать команду, которая не выполнится.
func validPackageName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	if strings.ContainsAny(name, " \t\n\r") {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		case r == '-' || r == '_' || r == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// donorCandidates — какие имена имеет смысл искать в PATH для донора.
//
// Имя пакета — не обязательно имя бинаря: scoutsuite даёт scout, oletools —
// olevba, wapiti3 — wapiti. Поэтому к имени пакета добавляются литералы
// shutil.which из кода самого плагина: если плагин ищет конкретное имя, мы
// проверяем именно его, а не угадываем по шаблону.
func donorCandidates(donor, pluginDir string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(donor)
	for _, w := range whichLiterals(pluginDir) {
		add(w)
	}
	return out
}

// whichLiterals — имена из shutil.which("…") по всем .py плагина.
func whichLiterals(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == "__pycache__" || name == ".git" {
				continue
			}
			out = append(out, whichLiterals(filepath.Join(dir, name))...)
			continue
		}
		if !strings.HasSuffix(name, ".py") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, m := range whichRe.FindAllStringSubmatch(string(raw), -1) {
			if len(m) > 1 {
				out = append(out, m[1])
			}
		}
	}
	return out
}

// donorInPath — есть ли такой донор в PATH.
//
// exec.LookPath, а не обход PATH вручную: он учитывает PATHEXT на Windows, то
// есть находит donor.exe и donor.cmd так же, как это делает плагин. Имя
// возвращается, чтобы показать человеку, КАКОЕ ИМЯ нашлось: у пакета
// scoutsuite бинарь называется scout, и «нашлось scout» полезнее, чем молчание.
func donorInPath(candidates []string) (found bool, foundName string) {
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return true, c
		}
	}
	return false, ""
}

// pinVersionRe — версия в пине `package==version`.
//
// Знак цифры на первом месте отсекает мусор: версия начинается с цифры, а
// «1.0;python_version<"3.9"» (маркер окружения) или «1.0=y» под неё не
// подходят — такой пин не разбирается и донор остаётся непроверенным.
var pinVersionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.*+!-]*$`)

// packageNameFromPin — имя пакета из строки вида `package==version`.
//
// Строгая сторона: отбрасывается только `==` + версия, и только если с левой
// стороны САМОСТОЯТЕЛЬНО валидное имя пакета. Обрезание «всё до равных знаков»
// было бы ровно тем багом, который здесь и чинится. Мусор вроде `vt-py):` или
// обратной кавычки в `webanalyze` равных знаков не содержит и ловится другими
// правилами, но `git+https://…==1.0` обрезался бы до `git+https://…` и больше
// не отсекался бы префиксом VCS — то есть враньё просто переехало бы на другой
// адрес. Поэтому несовпавшая строка отвергается целиком: лучше «донор НЕ
// проверен», чем выдуманное имя.
func packageNameFromPin(s string) (string, bool) {
	i := strings.Index(s, "==")
	if i < 0 {
		return s, false
	}
	name, version := s[:i], s[i+2:]
	if !validPackageName(name) || !pinVersionRe.MatchString(version) {
		return "", false
	}
	return name, true
}

// requirementName — имя пакета из спецификации `package==version`.
//
// Точные версии НЕ приводятся: задача и контракт этого не требуют, а пин
// меняется вместе с lock-файлом, и дублировать его здесь — значит показать
// человеку цифру, которая завтра разойдётся с манифестом.
func requirementName(spec string) string {
	s := strings.TrimSpace(spec)
	for _, sep := range []string{"==", ">=", "<=", "~=", "!=", ">", "<", "[", ";", "@"} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	return strings.Trim(s, "[] ")
}

// requirementImportName — имя МОДУЛЯ, которым этот пакет импортируется.
//
// Оно не всегда совпадает с именем пакета на PyPI: dnspython ставит модуль
// `dns`, и без этой поправки doctor объявил бы отсутствующим пакет, который на
// машине стоит и с которым плагин работает. Проверено: importlib.util.find_spec
// для «dnspython» возвращает None, для «dns» — модуль.
//
// Таблица намеренно мала и перечислимая: выводить её из кода плагинов дороже,
// чем поддерживать одну-две Known-истины, а выдумывать правила перевода имён
// («убери дефис», «замени _ на -») нельзя — они ломают имена вроде py-altdns.
var requirementImportAliases = map[string]string{
	"dnspython": "dns",
}

// requirementImportNames — под какими именами искать пакет в интерпретаторе:
// своё имя и, если известно отличие, имя модуля.
func requirementImportNames(name string) []string {
	if alias, ok := requirementImportAliases[name]; ok {
		return []string{name, alias}
	}
	return []string{name}
}

func installCommand(reqs []string) string {
	if len(reqs) == 0 {
		return ""
	}
	return "pip install " + strings.Join(reqs, " ")
}

// doctorSummary — одна строка человеческим языком.
func doctorSummary(rep doctorReport) string {
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

func printDoctorHuman(rep doctorReport) {
	printDoctorHumanTo(os.Stdout, rep)
}

// printDoctorHumanTo — вывод в произвольный writer.
//
// Отдельная функция, а не печать в stdout прямо: иначе оговорку о границах
// проверки нечем проверить тестом — её появление в выводе человек увидит
// сразу, а CI нет.
func printDoctorHumanTo(w io.Writer, rep doctorReport) {
	fmt.Fprintln(w, "WEDRA doctor — что сломано в окружении и что с этим делать")
	fmt.Fprintln(w)
	if rep.PythonError != "" {
		fmt.Fprintln(w, "Python: НЕ НАЙДЕН —", rep.PythonError)
		fmt.Fprintln(w, "       все 99 плагинов написаны на Python, без него не запустится ни один.")
	} else {
		fmt.Fprintf(w, "Python: %s\n", rep.Python.Version)
		fmt.Fprintf(w, "        %s\n", rep.Python.Path)
	}
	fmt.Fprintln(w)

	if rep.Total == 0 {
		fmt.Fprintln(w, "Плагины: не найдены в", rep.PluginsDir)
		fmt.Fprintln(w)
		fmt.Fprintln(w, rep.Summary)
		return
	}

	// Сначала те, кому что-то нужно: вверху то, что человек будет делать.
	var needDep, needDonor, ready []*doctorPlugin
	for i := range rep.Plugins {
		p := &rep.Plugins[i]
		switch p.Status {
		case doctorNeedsDep:
			needDep = append(needDep, p)
		case doctorNeedsDon:
			needDonor = append(needDonor, p)
		default:
			ready = append(ready, p)
		}
	}

	if len(needDep) > 0 {
		fmt.Fprintf(w, "Нужна pip-зависимость (%d): runtime.requires объявлен, но пакета нет\n", len(needDep))
		for _, p := range needDep {
			fmt.Fprintf(w, "  %-24s %s\n", p.ID, strings.Join(p.MissingRequires, ", "))
			fmt.Fprintf(w, "      %s\n", p.Install)
		}
		fmt.Fprintln(w)
	}
	if len(needDonor) > 0 {
		// Дефект 1: три разные ситуации требуют разного совета, и смешивать их
		// нельзя — «pip install X» для уже установленного пакета хуже
		// отсутствия совета: человек выполнит его и ничего не изменится.
		var missing, installedNoScript, unknown []*doctorPlugin
		for _, p := range needDonor {
			switch {
			case p.DonorInstalledNoScript:
				installedNoScript = append(installedNoScript, p)
			case p.DonorUnknown:
				unknown = append(unknown, p)
			default:
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			// Доноров больше, чем зависимостей, и они повторяются, поэтому
			// группируем по команде установки.
			byInstall := map[string][]string{}
			for _, p := range missing {
				byInstall[p.Install] = append(byInstall[p.Install], p.ID)
			}
			cmds := make([]string, 0, len(byInstall))
			for c := range byInstall {
				cmds = append(cmds, c)
			}
			sort.Strings(cmds)
			fmt.Fprintf(w, "Не установлен внешний инструмент (%d плагинов, %d команд):\n",
				len(missing), len(cmds))
			fmt.Fprintln(w, "runtime.requires его НЕ ставит — это отдельная установка.")
			for _, c := range cmds {
				ids := byInstall[c]
				sort.Strings(ids)
				fmt.Fprintf(w, "  %s\n", c)
				fmt.Fprintf(w, "      %s\n", strings.Join(ids, ", "))
			}
			fmt.Fprintln(w)
		}
		if len(installedNoScript) > 0 {
			fmt.Fprintf(w, "Пакет установлен, но его скрипта нет в PATH (%d):\n", len(installedNoScript))
			fmt.Fprintln(w, "Устанавливать НЕ нужно — пакет уже стоит. Не хватает каталога в PATH.")
			if rep.Python.ScriptsDir != "" && !rep.Python.ScriptsDirInPATH {
				fmt.Fprintf(w, "  добавь в PATH: %s\n", rep.Python.ScriptsDir)
			}
			for _, p := range installedNoScript {
				line := p.ID
				if len(p.DonorScripts) > 0 {
					line += "  (скрипт пакета: " + strings.Join(p.DonorScripts, ", ") + ")"
				}
				fmt.Fprintf(w, "  %s\n", line)
			}
			fmt.Fprintln(w)
		}
		if len(unknown) > 0 {
			fmt.Fprintf(w, "Не удалось надёжно определить наличие (%d) — проверь вручную:\n", len(unknown))
			for _, p := range unknown {
				fmt.Fprintf(w, "  %-24s %s\n", p.ID, p.Install)
			}
			fmt.Fprintln(w)
		}
	}
	if len(ready) > 0 {
		fmt.Fprintf(w, "Готовы сразу (%d):\n ", len(ready))
		for i, p := range ready {
			if i > 0 && i%8 == 0 {
				fmt.Fprint(w, "\n ")
			} else if i > 0 {
				fmt.Fprint(w, " ")
			}
			fmt.Fprint(w, p.ID)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w)
	}
	// Оговорка о границах проверки. Без неё вывод врёт: «готовы» звучит как
	// «всё настроено», а проверены только те доноры, что названы в описании
	// манифеста. Плагины из списка ниже НЕ проверены.
	if len(rep.UnverifiedDonors) > 0 {
		fmt.Fprintln(w, "Оговорка: проверены не все внешние инструменты — только те, что упомянуты")
		fmt.Fprintln(w, "в описании манифеста. У этих плагинов донора в описании нет,")
		fmt.Fprintln(w, "и наличие инструмента для них НЕ проверено:")
		fmt.Fprintf(w, "  %s\n", strings.Join(rep.UnverifiedDonors, ", "))
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, rep.Summary)
}
