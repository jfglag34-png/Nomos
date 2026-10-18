// Package doctor: ответ на вопрос «я скачал бинарь, ничего не настроил, что мне
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
//
// Почему пакет отдельный, а не функция внутри internal/cli: отчёт нужен не
// только терминалу. Магазин плагинов в GUI показывает те же данные, а
// internal/cli импортирует internal/api (см. internal/cli/gui.go) — значит
// импорт cli из api дал бы цикл и данные в веб-интерфейсе показать было бы
// нечем. Ядро поэтому лежит здесь, а в cli остались разбор флагов, печать и
// код выхода.
//
// Пакет ничего не печатает сам: PrintHuman пишет в переданный writer, stdout
// выбирает вызывающий (в cli это os.Stdout). Иначе тест не смог бы проверить
// оговорку о границах проверки, а она видна человеку только в выводе.
package doctor

import (
	"strings"

	"github.com/wykserdex/wedra/internal/plugin"
)

// Статусы плагина. Это НЕ коды ошибок протокола: они не попадают ни в журнал
// рана, ни в issues валидации, и намеренно не имеют вида E_*/W_*, чтобы гейт
// errcodes (коды в Go ↔ protocol/v0.2/ERRORS.md) не считал их контрактом.
//
// Значения строк — это JSON-контракт `wedra doctor --json`: вывод документирован
// и используется в README, поэтому переименование переменной не должно было
// тронуть ни одну букву значения.
const (
	StatusReady             = "ready"
	StatusNeedsDependency   = "needs_dependency"
	StatusNeedsExternalTool = "needs_external_tool"
)

// Run — полная проба окружения. pluginsDir пустой → каталог `plugins`
// относительно текущего рабочего каталога.
//
// Ошибок не возвращает и не накапливает: неуспех выражен в самом отчёте
// (OK=false, PythonError, счётчики NeedsDep/NeedsDonor), потому что у
// команды один результат, а не список причин.
func Run(pluginsDir string) Report {
	if pluginsDir == "" {
		pluginsDir = "plugins"
	}
	rep := Report{PluginsDir: pluginsDir, OK: true}

	// 1. Интерпретатор — тем же кодом, каким реально стартуют плагины.
	info, err := plugin.PythonInfo()
	if err != nil {
		rep.PythonError = err.Error()
		rep.OK = false
	} else {
		rep.Python = info
	}

	manifests := scanManifests(pluginsDir)
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
	plugins := make([]Plugin, 0, len(manifests))
	for _, m := range manifests {
		p := Plugin{
			ID:       m.ID,
			Dir:      m.Dir,
			Status:   StatusReady,
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
			p.Status = StatusNeedsDependency
			p.Install = installCommand(p.MissingRequires)
		}

		// Донор: «pip install X» из описания манифеста, fallback — README/.py.
		donor, donorOk := donorFromDescription(m.Description)
		if !donorOk {
			donor, donorOk = donorFromFiles(m.Dir)
		}
		if donorOk {
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
				p.DonorKind = DonorKindSameAsRequires
			} else {
				p.DonorKind = DonorKindExternal
			}

			if !foundPath {
				switch {
				case sameAsRequires:
					// Пакет из requires; статус уже выставлен выше по
					// MissingRequires. Отдельно ничего не советуем, чтобы не
					// предлагать одно и то же дважды.
					if p.Status == StatusReady {
						// Объявлен в requires, проба показала, что он есть, но
						// в PATH нет одноимённого бинаря: плагин запускает пакет
						// сам (паттерн C), поэтому бинарь ему не нужен.
						p.Status = StatusReady
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
						p.idxState = indexLookup(donor)
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
					if p.Status == StatusReady {
						p.Status = StatusNeedsExternalTool
					}
				}
			}
			p.DonorFound = foundPath
			if foundPath {
				p.DonorFoundName = foundName
			}
			if p.Status == StatusNeedsExternalTool {
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
		case StatusReady:
			rep.Ready++
		case StatusNeedsDependency:
			rep.NeedsDep++
		case StatusNeedsExternalTool:
			rep.NeedsDonor++
		}
		plugins = append(plugins, p)
	}

	rep.UnverifiedDonors = unverifiedDonorPlugins(manifests, plugins)

	rep.Plugins = plugins
	rep.Summary = summary(rep)
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

func installCommand(reqs []string) string {
	if len(reqs) == 0 {
		return ""
	}
	return "pip install " + strings.Join(reqs, " ")
}
