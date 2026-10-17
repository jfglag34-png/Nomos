package doctor

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// PrintHuman — отчёт человеческим языком в переданный writer.
//
// Writer, а не os.Stdout: пакет ничего не печатает сам (это правило, а не
// случайность — иначе вызов из GUI или из теста не смог бы ни проверить текст,
// ни отдать его в HTTP-ответ). В cli вызов остаётся с os.Stdout.
func PrintHuman(w io.Writer, rep Report) {
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
	var needDep, needDonor, ready []*Plugin
	for i := range rep.Plugins {
		p := &rep.Plugins[i]
		switch p.Status {
		case StatusNeedsDependency:
			needDep = append(needDep, p)
		case StatusNeedsExternalTool:
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
		var missing, installedNoScript, unknown []*Plugin
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
