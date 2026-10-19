package cli

// `wedra check` — единая точка входа для проверки проекта.
//
// Зачем она, если есть Makefile: Makefile написан на юниксовой оболочке
// (`test -z`, `for … do`, `rm -f`), то есть на Windows не работает вовсе, и
// там каждый шаг приходилось бы вбивать руками. Команда на Go одинаково
// идёт на всех платформах и печатает одну сводку.
//
// Смысл в том, чтобы это был ЕДИНСТВЕННЫЙ источник правды о проверке: те же
// шаги, что и в CI, в том же порядке. Команда, которую CI зовёт сам, иначе
// была бы четвёртой копией одной и той же последовательности.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wykserdex/wedra/internal/changelog"
	"github.com/wykserdex/wedra/internal/errdoc"
	"github.com/wykserdex/wedra/internal/schemacheck"
	"github.com/wykserdex/wedra/internal/supply"
	"github.com/wykserdex/wedra/internal/verdoc"
)

type checkStep struct {
	// name — как шаг называется в выводе и в --only
	name string
	// desc — одна строка в --list
	desc string
	// fast — можно пропустить по --fast (быстрые шаги не трогаем)
	fast bool
	// run возвращает вывод шага и ошибку
	run func(opts checkOpts) (string, error)
}

type checkOpts struct {
	repo    string
	verbose bool
	// testTimeout — сколько секунд на весь go test
	testTimeout int
	// census вместо обычного теста: поштучно по пакетам с пределом на пакет
	census        bool
	pkgFilter     string
	race          bool
	perPackageSec int
	goTimeoutSec  int
}

func checkSteps() []checkStep {
	return []checkStep{
		{name: "fmt", desc: "gofmt -l по исходникам", fast: true,
			run: func(o checkOpts) (string, error) {
				// Проверяем то же, что проверяет CI, плюс все cmd-каталоги.
				out, err := runCapture(o.repo, "gofmt", "-l", "./internal", "./cmd")
				if err != nil {
					return out, err
				}
				if strings.TrimSpace(out) != "" {
					return out, fmt.Errorf("исходники не отформатированы: %s", strings.TrimSpace(out))
				}
				return "исходники отформатированы", nil
			}},
		{name: "vet", desc: "go vet ./...", fast: true,
			run: func(o checkOpts) (string, error) {
				return runCapture(o.repo, "go", "vet", "./...")
			}}, {name: "lint", desc: "staticcheck ./...", fast: true,
			run: func(o checkOpts) (string, error) {
				bin, err := staticcheckBin()
				if err != nil {
					// Не пропуск: «пропуск неотличим от успеха» — правило самого
					// проекта. Отсутствие линтера — отсутствие проверки.
					return "", err
				}
				return runCapture(o.repo, bin, "./...")
			}},

		{name: "mod", desc: "проверка целостности go.mod/go.sum", fast: true,
			run: func(o checkOpts) (string, error) {
				return runCapture(o.repo, "go", "mod", "verify")
			}},
		{name: "versions", desc: "VERSION ↔ README и минимум Go ↔ go.mod", fast: true,
			run: func(o checkOpts) (string, error) {
				return verdoc.CheckRepo(o.repo)
			}},
		// Структура ченджлога. Единственное место в репозитории, где ошибка
		// не ломает сборку, но молча выбрасывает написанное: потерянный
		// заголовок раздела выглядит ровно так же, как «никто ничего не менял».
		{name: "changelog", desc: "структура CHANGELOG.md: разделы версий и подразделы", fast: true,
			run: func(o checkOpts) (string, error) {
				return changelog.CheckRepo(o.repo)
			}},
		{name: "errcodes", desc: "коды ошибок в Go ↔ protocol/v0.2/ERRORS.md", fast: true,
			run: func(o checkOpts) (string, error) {
				return errdoc.CheckRepo(o.repo)
			}},
		{name: "schemas", desc: "schemas/ против примеров и манифестов", fast: true,
			run: func(o checkOpts) (string, error) {
				return schemacheck.CheckRepo(o.repo)
			}},
		// Цепочка поставки. Проверка статическая: она не трогает криптографию
		// и не может доказать, что подпись верна — это делает cosign в
		// release.yml. Она ловит другое и ловит раньше всех: шаг подписи или
		// закрепление actions удаляются без единого красного прогона, если
		// следить за этим некому.
		{name: "supply", desc: "actions по SHA + подпись и проверка подписи в релизном воркфлоу", fast: true,
			run: func(o checkOpts) (string, error) {
				return supply.CheckRepo(o.repo)
			}},
		{name: "build", desc: "сборка всех точек входа", fast: false,
			run: func(o checkOpts) (string, error) {
				bin := filepath.Join(o.repo, "var", "check")
				if err := os.MkdirAll(bin, 0o755); err != nil {
					return "", err
				}
				var notes []string
				for _, target := range []string{"./cmd/wedra", "./cmd/wedragui", "./cmd/tool"} {
					out, err := runCapture(o.repo, "go", "build", "-o",
						filepath.Join(bin, "wedra-check-bin-"+filepath.Base(target)+exeExt()), target)
					if err != nil {
						return strings.Join(notes, "\n"), fmt.Errorf("сборка %s: %w\n%s", target, err, out)
					}
					notes = append(notes, target+" собран")
				}
				return strings.Join(notes, "\n"), nil
			}},
		{name: "test", desc: "go test ./... (или поштучно по пакетам с --census)", fast: false,
			run: func(o checkOpts) (string, error) {
				if o.census {
					return runCensus(o)
				}
				args := []string{"test", "./...", "-count=1",
					"-timeout", strconv.Itoa(o.testTimeout) + "s"}
				if o.race {
					args = append(args, "-race")
				}
				return runCapture(o.repo, "go", args...)
			}},
		{name: "conformance", desc: "конформ фикстур плагинов", fast: false,
			run: func(o checkOpts) (string, error) {
				// -tags integration: TestPluginTestShippedPlugins запускает
				// Python и порождает процесс на каждый шипленный плагин, поэтому
				// он живёт в интеграционном тире, а не в юнит-пакете. Домом для
				// него был и раньше этот шаг (фильтр TestPluginTest), так что
				// покрытие не теряется — просто у него свой бюджет.
				return runCapture(o.repo, "go", "test", "-tags", "integration",
					"./internal/core/",
					"-run", "TestPluginTest|TestExec", "-count=1", "-timeout", strconv.Itoa(o.testTimeout)+"s")
			}},
		{name: "mcp", desc: "контракт MCP: транспорт, аннотации, схемы, elicitation", fast: false,
			run: func(o checkOpts) (string, error) {
				// Отдельный шаг, а не просто «входит в test»: контракт MCP —
				// внешний интерфейс, и его поломка (ревизия, форма ответа,
				// аннотации, отмена) должна называться по имени, а не тонуть
				// в общем прогоне. Тесты помечены TestProtocol||Spec: по этому
				// фильтру видно, что именно проверено.
				// Cancelled-тесты живут с реальными таймерами, поэтому шаг
				// ограничен своим бюджетом, а не общим.
				out, err := runCapture(o.repo, "go", "test", "./internal/mcp/", "-count=1", "-v",
					"-timeout", strconv.Itoa(o.testTimeout)+"s", "-run", "TestConformance")
				if err != nil {
					return out, err
				}
				passed, failed := 0, 0
				for _, line := range strings.Split(out, "\n") {
					switch {
					case strings.HasPrefix(line, "--- PASS"):
						passed++
					case strings.HasPrefix(line, "--- FAIL"):
						failed++
					}
				}
				// Сводка идёт первой строкой: check печатает начало вывода,
				// а хвост прогона тонет в «ещё N строк».
				return fmt.Sprintf("контракт MCP (ревизия 2025-06-18): пройдено %d, провалено %d\n%s",
					passed, failed, out), nil
			}},
		{name: "pipelines", desc: "validate + lint + plan для каждого examples/*.yaml", fast: false,
			run: func(o checkOpts) (string, error) { return checkPipelines(o) }},
		{name: "plugins", desc: "validate + test для каждого плагина", fast: false,
			run: func(o checkOpts) (string, error) { return checkPlugins(o) }},
		{name: "registry", desc: "validate registry.yaml с локальным источником", fast: true,
			run: func(o checkOpts) (string, error) {
				bin, err := ensureWedra(o)
				if err != nil {
					return "", err
				}
				return runCapture(o.repo, bin, "registry", "validate", "--registry=registry.yaml", "--local-source=.")
			}},
	}
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func checkBinary(repo string) string {
	return filepath.Join(repo, "var", "check", "wedra-check-bin-wedra"+exeExt())
}

// ensureWedra даёт путь к бинарю wedra, достраивая его при необходимости.
// Раньше шаги registry/pipelines/plugins брали бинарь из шага build и падали с
// невнятным «path not found», если его запускали отдельно (`--only=registry`,
// `--fast`). Порядок шагов перестал быть частью контракта.
func ensureWedra(o checkOpts) (string, error) {
	bin := checkBinary(o.repo)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	// Свежий бинарь не пересобираем: полная сборка всех трёх точек входа —
	// это шаг build, здесь достаточно одной.
	fresh := false
	if st, err := os.Stat(bin); err == nil {
		if src, err := newestSource(o.repo); err == nil && src.After(st.ModTime()) {
			fresh = false
		} else {
			fresh = true
		}
	}
	if fresh {
		return bin, nil
	}
	out, err := runCapture(o.repo, "go", "build", "-o", bin, "./cmd/wedra")
	if err != nil {
		return "", fmt.Errorf("сборка wedra: %w\n%s", err, out)
	}
	return bin, nil
}

// newestSource ищет самый свежий .go и go.mod: если исходник новее бинаря,
// бинарь надо пересобрать.
func newestSource(repo string) (time.Time, error) {
	var newest time.Time
	err := filepath.Walk(repo, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "var" || base == "plugins" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go":
		default:
			if info.Name() != "go.mod" && info.Name() != "go.sum" {
				return nil
			}
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

// runCapture запускает команду в каталоге репозитория и возвращает вывод.
// Ошибка возвращается вместе с выводом: без него диагностика невозможна.
func runCapture(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	// Смешивать stdout и stderr нельзя: go vet и go test пишут диагностику
	// в stderr, а сводка должна содержать всё.
	raw, err := cmd.CombinedOutput()
	return string(raw), err
}

func checkPipelines(o checkOpts) (string, error) {
	bin, err := ensureWedra(o)
	if err != nil {
		return "", err
	}
	files, err := filepath.Glob(filepath.Join(o.repo, "examples", "*.yaml"))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("в examples/ нет ни одного *.yaml")
	}
	sort.Strings(files)
	var failed []string
	for _, f := range files {
		for _, sub := range []string{"validate", "lint", "plan"} {
			out, err := runCapture(o.repo, bin, "pipeline", sub, f)
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s %s: %v", filepath.Base(f), sub, firstLine(out, err)))
			}
		}
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("не прошли: %s", strings.Join(failed, "; "))
	}
	return fmt.Sprintf("%d пайплайнов: validate+lint+plan", len(files)), nil
}

func checkPlugins(o checkOpts) (string, error) {
	bin, err := ensureWedra(o)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, pattern := range []string{
		filepath.Join(o.repo, "plugins", "official", "*"),
		filepath.Join(o.repo, "plugins", "community", "*"),
		filepath.Join(o.repo, "plugins", "agent-plugins", "*"),
	} {
		entries, _ := filepath.Glob(pattern)
		for _, d := range entries {
			if _, err := os.Stat(filepath.Join(d, "plugin.yaml")); err == nil {
				dirs = append(dirs, d)
			}
		}
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("не найдено плагинов с plugin.yaml")
	}
	sort.Strings(dirs)
	var failed []string
	for _, d := range dirs {
		for _, sub := range []string{"validate", "test"} {
			out, err := runCapture(o.repo, bin, "plugin", sub, d)
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s %s: %v", filepath.Base(d), sub, firstLine(out, err)))
			}
		}
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("не прошли: %s", strings.Join(failed, "; "))
	}
	return fmt.Sprintf("%d плагинов: validate+test", len(dirs)), nil
}

func firstLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			return s
		}
	}
	if err != nil {
		return err.Error()
	}
	return "(без вывода)"
}

type checkResult struct {
	Step string  `json:"step"`
	OK   bool    `json:"ok"`
	Secs float64 `json:"secs"`
	Note string  `json:"note"`
}

// RunCheck — единая точка входа. Возвращает код возврата для main.
func RunCheck(args []string) int {
	o := checkOpts{
		testTimeout:   900,
		perPackageSec: 200,
		goTimeoutSec:  150,
	}
	var only string
	var list bool
	var fast bool
	var asJSON bool
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--help" || a == "-h":
			printCheckHelp()
			return 0
		case a == "--list":
			list = true
		case a == "--fast":
			fast = true
		case a == "--verbose" || a == "-v":
			o.verbose = true
		case a == "--census":
			o.census = true
		case strings.HasPrefix(a, "--pkg="):
			o.pkgFilter = strings.TrimPrefix(a, "--pkg=")
		case strings.HasPrefix(a, "--only="):
			only = strings.TrimPrefix(a, "--only=")
		case strings.HasPrefix(a, "--timeout="):
			if v, err := strconv.Atoi(strings.TrimPrefix(a, "--timeout=")); err == nil && v > 0 {
				o.testTimeout = v
			}
		case strings.HasPrefix(a, "--per-package="):
			if v, err := strconv.Atoi(strings.TrimPrefix(a, "--per-package=")); err == nil && v > 0 {
				o.perPackageSec = v
			}
		case strings.HasPrefix(a, "--go-timeout="):
			if v, err := strconv.Atoi(strings.TrimPrefix(a, "--go-timeout=")); err == nil && v > 0 {
				o.goTimeoutSec = v
			}
		case a == "--json":
			asJSON = true
		case a == "--race":
			o.race = true
		default:
			fmt.Fprintf(os.Stderr, "check: неизвестный аргумент %q (см. --help)\n", a)
			return 2
		}
	}

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "check:", err)
		return 1
	}
	if root := findRepoRoot(wd); root != "" {
		o.repo = root
	} else {
		o.repo = wd
	}

	steps := checkSteps()
	if list {
		fmt.Fprintf(os.Stderr, "wedra check — шаги (в том же порядке, что в CI):\n")
		for _, s := range steps {
			tag := "медленный"
			if s.fast {
				tag = "быстрый"
			}
			fmt.Fprintf(os.Stderr, "  %-12s %-8s %s\n", s.name, tag, s.desc)
		}
		return 0
	}
	if only != "" {
		var kept []checkStep
		for _, s := range steps {
			if s.name == only {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			fmt.Fprintf(os.Stderr, "check: нет шага %q (см. --list)\n", only)
			return 2
		}
		steps = kept
	}
	if fast {
		var kept []checkStep
		for _, s := range steps {
			if s.fast {
				kept = append(kept, s)
			}
		}
		steps = kept
	}

	fmt.Printf("wedra check: %s\n", o.repo)
	if o.census {
		fmt.Printf("режим census: поштучно по пакетам, предел %d с на пакет, -timeout %d с\n",
			o.perPackageSec, o.goTimeoutSec)
	}
	results := make([]checkResult, 0, len(steps))
	failed := 0
	for _, s := range steps {
		fmt.Printf("\n== %-12s %s\n", s.name, s.desc)
		t0 := time.Now()
		out, err := s.run(o)
		took := time.Since(t0)
		res := checkResult{Step: s.name, OK: err == nil, Secs: took.Round(time.Millisecond).Seconds()}
		if err != nil {
			failed++
			res.Note = firstLine(out, err)
			fmt.Fprintf(os.Stderr, "   FAIL за %s: %v\n", took.Round(time.Millisecond), err)
			printDetail(out, o.verbose)
		} else {
			if note := singleLine(out); note != "" {
				res.Note = note
				fmt.Printf("   ok за %s: %s\n", took.Round(time.Millisecond), note)
			} else {
				fmt.Printf("   ok за %s\n", took.Round(time.Millisecond))
			}
			if o.verbose && strings.TrimSpace(out) != "" {
				printDetail(out, true)
			}
		}
		results = append(results, res)
	}

	blob, jerr := json.MarshalIndent(results, "", "  ")
	if jerr == nil {
		_ = os.WriteFile(filepath.Join(o.repo, "var", "check", "last-check.json"), blob, 0o644)
	}

	if asJSON {
		// В --json сводка идёт в stdout, а ход работы шагов — в stderr,
		// чтобы вывод можно было пипать, не вылавливая строки прогресса.
		os.Stdout.Write(append(blob, '\n'))
		if failed > 0 {
			return 1
		}
		return 0
	}

	fmt.Printf("\n=== итог ===\n")
	for _, r := range results {
		mark := "ok  "
		if !r.OK {
			mark = "FAIL"
		}
		fmt.Printf("  %s  %-12s %8.2fs  %s\n", mark, r.Step, r.Secs, r.Note)
	}
	if failed > 0 {
		fmt.Printf("провалено шагов: %d из %d\n", failed, len(results))
	} else {
		fmt.Printf("все %d шагов прошли\n", len(results))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func singleLine(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(l); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	// Одна строка: всё подробное уже напечатано выше по шагу, а таблица
	// нужна, чтобы одним взглядом увидеть, где именно сломалось.
	if len(lines) > 1 {
		return lines[0] + fmt.Sprintf(" … ещё %d строк", len(lines)-1)
	}
	return lines[0]
}

func printDetail(out string, verbose bool) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		return
	}
	if !verbose && len(lines) > 25 {
		lines = append(lines[:25], fmt.Sprintf("… ещё %d строк (полностью: --verbose)", len(lines)-25))
	}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			fmt.Fprintln(os.Stderr, "     |", l)
		}
	}
}

// findRepoRoot идёт вверх до каталога с go.mod: `wedra check` должен
// работать из любого подкаталога, как и CI, который всегда в корне.
func findRepoRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func printCheckHelp() {
	fmt.Fprint(os.Stderr, `wedra check — единая точка проверки проекта.

Шаги идут в том же порядке, что в CI, и печатаются одной сводкой:
  fmt          gofmt -l по исходникам
  vet          go vet ./...
  mod          go mod verify
  versions     VERSION ↔ README/docs, минимум Go ↔ директива в go.mod
  errcodes     коды ошибок в Go ↔ protocol/v0.2/ERRORS.md
  schemas      schemas/ против примеров и манифестов
  supply       actions по SHA + подпись и проверка подписи в release.yml
  build        сборка cmd/wedra, cmd/wedragui, cmd/tool
  test         go test ./... (с --census — поштучно по пакетам)
  conformance  конформ фикстур плагинов
  pipelines    validate + lint + plan для examples/*.yaml
  plugins      validate + test для каждого плагина
  registry     validate registry.yaml --local-source=.

Флаги:
  --list              показать шаги и выйти
  --only=<step>       прогнать один шаг
  --fast              только быстрые шаги (fmt, vet, mod, versions, errcodes, schemas, supply, registry)
  --census            тесты поштучно по пакетам с пределом на пакет
  --pkg=<substr>      в census ограничить список пакетов подстрокой
  --per-package=<s>   предел на пакет в режиме census (по умолчанию 200)
  --go-timeout=<s>    -timeout для go test в census (по умолчанию 150)
  --timeout=<s>       -timeout для обычного go test (по умолчанию 900)
  --verbose           печатать вывод шага целиком
  --race              go test с -race, как в CI
  --json              только сводка в stdout, ход шагов в stderr (всегда ещё var/check/last-check.json)

Шаг registry сверяет commit-пины с историей git, поэтому на мелкой копии
(git clone --depth 1) он упадёт с «Not a valid commit name». Лечится
git fetch --unshallow, а не отключением шага.

Примеры:
  wedra check --fast          перед коммитом
  wedra check                 полный прогон, как в CI
  wedra check --race          то же с -race, ровно как в CI
  wedra check --census        охота за зависанием Windows-джоба
`)
}

// staticcheckPin — версия линтера, которую ставит CI. Пин в одном месте: если
// версии разъедутся, локальная зелёная проверка и CI будут говорить о разном
// коде разное.
const staticcheckPin = "2026.2.1"

// staticcheckBin ищет staticcheck в PATH: своим шагом он не тянет зависимость в
// go.mod (а значит, не попадает в SBOM и релизные бинарники), но и молча не
// пропускается, когда его нет.
func staticcheckBin() (string, error) {
	if p, err := exec.LookPath("staticcheck"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("staticcheck не найден в PATH: локальная проверка без него "+
		"неполна, а «пропуск неотличим от успеха». Поставьте: go install honnef.co/go/tools/cmd/staticcheck@%s",
		staticcheckPin)
}
