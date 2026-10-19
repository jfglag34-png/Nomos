package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/wykserdex/wedra/internal/core"
)

func RunPluginValidate(args []string) {
	if len(args) < 1 {
		fmt.Println("нужен путь к плагину: wedra plugin validate <dir>")
		os.Exit(2)
	}
	errs := core.ValidatePluginDir(args[0])
	for _, e := range errs {
		fmt.Println("  ✗", e)
	}
	if len(errs) > 0 {
		os.Exit(1)
	}
	for _, w := range core.PluginManifestWarnings(args[0]) {
		fmt.Println("  · предупреждение:", w)
	}
	fmt.Println("OK: манифест плагина корректен")
}

func RunPluginTest(args []string) {
	dir := ""
	spec := ""
	fixtures := ""
	conformance, asJSON := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--spec":
			if i+1 >= len(args) {
				fmt.Println("флагу --spec нужно значение")
				os.Exit(2)
			}
			i++
			spec = args[i]
		case strings.HasPrefix(a, "--spec="):
			spec = strings.TrimPrefix(a, "--spec=")
		case a == "--fixtures":
			if i+1 >= len(args) {
				fmt.Println("флагу --fixtures нужно значение")
				os.Exit(2)
			}
			i++
			fixtures = args[i]
		case strings.HasPrefix(a, "--fixtures="):
			fixtures = strings.TrimPrefix(a, "--fixtures=")
		case a == "--conformance":
			conformance = true
		case a == "--json":
			asJSON = true
		case a == "--help" || a == "-h":
			fmt.Println("wedra plugin test <dir> [--spec file.yaml]")
			fmt.Println("wedra plugin test --conformance [--json] [--fixtures=dir]")
			return
		case strings.HasPrefix(a, "-"):
			fmt.Printf("неизвестный флаг plugin test %q\n", a)
			os.Exit(2)
		case dir == "":
			dir = a
		default:
			fmt.Printf("лишний аргумент plugin test %q\n", a)
			os.Exit(2)
		}
	}
	if !conformance && dir == "" {
		fmt.Println("нужен путь к плагину: wedra plugin test <dir>")
		os.Exit(2)
	}
	if conformance {
		if fixtures != "" {
			dir = fixtures
		}
		report := core.RunConformance(dir)
		if asJSON {
			b, _ := json.MarshalIndent(report, "", "  ")
			fmt.Println(string(b))
		} else {
			for _, c := range report.Checks {
				mark := "✓"
				if !c.Pass {
					mark = "✗"
				}
				fmt.Printf("  %s %-12s %s\n", mark, c.Name, c.Details)
			}
		}
		if !report.OK {
			os.Exit(1)
		}
		return
	}
	fmt.Printf("▶ plugin test %s\n", dir)
	passed, failed, err := core.RunPluginTests(dir, spec, false)
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	fmt.Printf("■ plugin test: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// printPluginCreateUsage — справка именно по `plugin create`.
//
// Раньше `wedra plugin create --help` печатал глобальную справку: root ловил
// `--help` где угодно в аргументах, и подкоманда до своей справки не доходила.
// Человек видел список команд WEDRA вместо того, как создать плагин, и
// справка выглядела сломанной.
func printPluginCreateUsage() {
	fmt.Println(`wedra plugin create <dir> [--author=ИМЯ] [--description=ТЕКСТ]
                      [--example=JSON] [--template=ИМЯ] [--pattern=РЕГУЛЯРКА]
                      [--replacement=ТЕКСТ] [--list-templates]

Создаёт каталог плагина: plugin.yaml + main.py + plugin.test.yaml + README.md
и requirements.lock (если плагину нужен донор). Готовый каталог сразу проходит
проверку ValidatePluginDir и его тесты.

--template=ИМЯ      шаблон тела: skeleton, rest-api, smtp, postgres, redis
--pattern/--replacement   правило замены в теле (например, переименовать
                     входной порт) — применяется к сгенерированному main.py
--list-templates    показать список шаблонов и выйти
--author/--description/--example   метаданные плагина

Путь каталога обязателен и должен быть новым.`)
}

// RunPluginCreate — wedra plugin create <dir> [...]
func RunPluginCreate(args []string) {
	if helpRequested(args) {
		printPluginCreateUsage()
		return
	}
	dir, opts, listRequested, err := core.ParseCreateArgs(args)
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(2)
	}
	if listRequested {
		fmt.Print(core.ListTemplates())
		return
	}
	id, err := core.CreatePluginWith(dir, opts)
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	tplName := opts.Template
	if tplName == "" {
		tplName = "skeleton"
	}
	fmt.Printf("▶ создан плагин %s → %s (шаблон %s)\n", id, dir, tplName)
	fmt.Println("  plugin.yaml + main.py + plugin.test.yaml + README.md")

	if errs := core.ValidatePluginDir(dir); len(errs) > 0 {
		fmt.Println("внутренняя ошибка генератора — скелет невалиден:")
		for _, e := range errs {
			fmt.Println("  ✗", e)
		}
		os.Exit(1)
	}
	fmt.Println("  ✓ манифест валиден")

	passed, failed, err := core.RunPluginTests(dir, "", false)
	if err != nil {
		fmt.Printf("  · тесты не запустились (%v) — проверьте python3\n", err)
	} else if failed > 0 {
		fmt.Println("внутренняя ошибка генератора — стартовые тесты красные")
		os.Exit(1)
	} else {
		fmt.Printf("  ✓ %d стартовых теста зелёные\n", passed)
	}
	if tplName == "skeleton" {
		fmt.Printf("\nДальше: правьте %s/main.py, затем wedra plugin test %s\n", dir, dir)
		return
	}
	// Шаблон даёт рабочий плагин, поэтому «дальше» — это уже не написание
	// кода, а настройка. Формулировка шаблона иначе обещала бы то, чего
	// от него не ждут.
	fmt.Printf("\nПлагин рабочий. Дальше: %s\n", templateNextStep(tplName, dir))
}

func templateNextStep(tplName, dir string) string {
	switch tplName {
	case "regex-replace":
		return "поправьте PATTERN и REPLACEMENT в " + dir + "/main.py, " +
			"затем подставьте точный результат в plugin.test.yaml и прогоните wedra plugin test " + dir
	case "line-filter":
		return "поправьте PATTERN в " + dir + "/main.py, затем прогоните wedra plugin test " + dir
	default:
		return "расширяйте вывод в " + dir + "/main.py и в том же порядке в манифесте и plugin.test.yaml"
	}
}

func RunPluginInspect(args []string) {
	if len(args) < 1 {
		fmt.Println("нужен путь к плагину: wedra plugin inspect <dir>")
		os.Exit(2)
	}
	eng := core.NewEngine()
	m, err := eng.LoadManifest(args[0])
	if err != nil {
		fmt.Println("ошибка:", err)
		os.Exit(1)
	}
	fmt.Printf("Plugin: %s\n", m.ID)
	fmt.Printf("Version: %s (platform_api %s)\n", m.Version, m.PlatformAPI)
	fmt.Printf("Description: %s\n", m.Description)
	fmt.Printf("Author: %s\n", m.Author)
	fmt.Printf("Runtime: %s %s\n", m.Runtime.Type, m.Runtime.Entry)
	fmt.Println("Input:")
	for k, p := range m.Input {
		fmt.Printf("  %s: type=%s format=%s optional=%v from=%s\n", k, p.Type, p.Format, p.Optional, p.From)
	}
	fmt.Println("Output:")
	for k, p := range m.Output {
		fmt.Printf("  %s: type=%s format=%s optional=%v\n", k, p.Type, p.Format, p.Optional)
	}
	fmt.Printf("Permissions: %+v\n", m.Permissions)
}

func RunPluginList(args []string) {
	plugins := core.ScanPlugins()
	if len(plugins) == 0 {
		fmt.Println("плагины не найдены в plugins (flat/official/community)")
		return
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].ID < plugins[j].ID })
	fmt.Printf("Найдено %d плагинов:\n", len(plugins))
	for _, p := range plugins {
		fmt.Printf("  %-24s %-10s %s (%s)\n", p.ID, p.Version, p.Dir, p.Description)
	}
}

func RunPluginSearch(args []string) {
	if len(args) < 1 {
		fmt.Println("нужен запрос: wedra plugin search <query>")
		os.Exit(2)
	}
	query := strings.ToLower(strings.Join(args, " "))
	plugins := core.ScanPlugins()
	matched := []core.Manifest{}
	for _, p := range plugins {
		hay := strings.ToLower(p.ID + " " + p.Description + " " + p.Author)
		if strings.Contains(hay, query) {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		fmt.Printf("по запросу %q ничего не найдено (всего %d плагинов)\n", query, len(plugins))
		return
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })
	fmt.Printf("По запросу %q найдено %d:\n", query, len(matched))
	for _, p := range matched {
		fmt.Printf("  %-24s %-10s %s\n    %s\n", p.ID, p.Version, p.Dir, p.Description)
	}
}
