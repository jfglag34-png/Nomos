package importer

// Тесты второго слоя импортёра: генерация каталога плагина по плану.
//
// Проверяется не «сгенерировалось что-то», а критерий приёмки целиком:
// ValidatePluginDir даёт ноль ошибок и RunPluginTests даёт passed > 0 при
// failed == 0 на плане, собранном из РЕАЛЬНОГО чужого проекта. Второй по
// важности класс проверок — отказы: генератор, который пишет поверх чужой
// папки или подставляет небезопасный id, опаснее генератора, который не
// пишет ничего.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wykserdex/wedra/internal/core"
)

// requirePython — прогон контракт-тестов зовёт настоящий интерпретатор.
// Пропуск без него повторяет соглашение internal/core/helpers_test.go: без
// Python проверять нечего, а падать из-за отсутствия чужой программы значит
// врать о состоянии кода.
func requirePython(t *testing.T) {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if _, err := exec.LookPath(name); err == nil {
			return
		}
	}
	t.Skip("python не найден — прогон плагина проверить нечем")
}

// otklikProject раскладывает настоящий pyproject.toml из otklik в отдельный
// каталог и добавляет к нему минимальную src-раскладку.
//
// Каталог нужен потому, что Inspect читает каталог, а критерий приёмки говорит
// про «проект из testdata/otklik_pyproject.toml через Inspect». Каталог src/
// добавляется с одной целью: чтобы раскладка была src-layout, и ModuleImportPath
// указывал на существующее место. Иначе план схлопнется в LayoutUnknown, и мы
// проверяли бы не генерацию, а отказ по неразрешённой раскладке.
func otklikProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("testdata", "otklik_pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "src", "telegram_outreach")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	// Точка входа донора — CLI, и импортёр обязан это увидеть (см. cliLikeTargets).
	for name, body := range map[string]string{
		"__init__.py": "",
		"main.py":     "def cli():\n    return 0\n",
	} {
		if err := os.WriteFile(filepath.Join(pkg, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func otklikPlan(t *testing.T) ImportPlan {
	t.Helper()
	p, err := Inspect(otklikProject(t))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	plan := Plan(p)
	if len(plan.Unresolved) == 0 {
		t.Fatal("ожидались неразрешённые зависимости: у otklik все версии заданы диапазоном")
	}
	if len(plan.EntryPoints) == 0 {
		t.Fatal("ожидалась хотя бы одна разобранная точка входа")
	}
	return plan
}

// TestGenerateOtklikIsRealPluginDir — критерий приёмки целиком, на реальном
// чужом проекте. Один этот тест ловит всё сразу: негодный манифест, адаптер,
// который падает вместо ответа, и тесты, которые красные из коробки.
func TestGenerateOtklikIsRealPluginDir(t *testing.T) {
	requirePython(t)

	plan := otklikPlan(t)
	dir := filepath.Join(t.TempDir(), "otklik_plugin")

	res, err := Generate(plan, dir, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if errs := core.ValidatePluginDir(dir); len(errs) > 0 {
		t.Fatalf("ValidatePluginDir вернул %d ошибок: %v", len(errs), errs)
	}
	// Числа критерия приёмки в лог: их должен видеть тот, кто смотрит прогон,
	// а не только тот, кто открыл тест.
	t.Logf("критерий: ValidatePluginDir ошибок=%d, files=%v", 0, res.Files)

	passed, failed, err := core.RunPluginTests(dir, "", false)
	if err != nil {
		t.Fatalf("RunPluginTests: %v", err)
	}
	t.Logf("критерий: RunPluginTests passed=%d failed=%d", passed, failed)
	if passed == 0 || failed != 0 {
		t.Fatalf("тесты: passed=%d failed=%d, ждали passed>0 и failed=0", passed, failed)
	}
	// internal/cli/registry.go не пускает запись с меньше чем тремя тестами:
	// сгенерированный плагин должен быть допустим реестром, иначе генерация
	// бесполезна ровно настолько, насколько бесполезен плагин без записи.
	if passed+failed < 3 {
		t.Fatalf("реестр требует минимум 3 теста (internal/cli/registry.go), сгенерировано %d", passed+failed)
	}

	if res.PluginID != plan.PluginID {
		t.Errorf("PluginID = %q, ждали %q", res.PluginID, plan.PluginID)
	}
	wantFiles := []string{"plugin.yaml", "main.py", "plugin.test.yaml", "README.md"}
	if strings.Join(res.Files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("записаны %v, ждали %v", res.Files, wantFiles)
	}
	for _, name := range wantFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s не записан: %v", name, err)
		}
	}
	// Плагин без блокеров на проекте с 12 незапиненными зависимостями — это
	// ровно то враньё, ради которого этот слой написан.
	if len(res.Blockers) == 0 {
		t.Error("Blockers пуст: проект с неразрешёнными зависимостями обязан что-то оставлять человеку")
	}
	for i, blocker := range res.Blockers {
		t.Logf("blocker[%d]: %s", i, blocker)
	}
}

// TestGenerateRefusesBadDest — отказ на целевом каталоге и, главное, отсутствие
// записи при отказе. Запись «почти всё» поверх чужой папки хуже отказа.
func TestGenerateRefusesBadDest(t *testing.T) {
	plan := otklikPlan(t)

	t.Run("пустой путь", func(t *testing.T) {
		if _, err := Generate(plan, "   ", GenerateOptions{}); err == nil {
			t.Fatal("ожидался отказ по пустому пути")
		}
	})
	t.Run("непустая папка", func(t *testing.T) {
		dir := t.TempDir()
		keep := filepath.Join(dir, "моя_работа.txt")
		if err := os.WriteFile(keep, []byte("не трогать"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Generate(plan, dir, GenerateOptions{}); err == nil {
			t.Fatal("ожидался отказ по непустой папке")
		} else if !strings.Contains(err.Error(), "не пуста") {
			t.Errorf("сообщение не объясняет причину: %v", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != "моя_работа.txt" {
			t.Errorf("в папке появилось лишнее: %v", entries)
		}
	})
	t.Run("путь — файл", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "plugin.yaml")
		if err := os.WriteFile(file, []byte("id: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Generate(plan, file, GenerateOptions{}); err == nil {
			t.Fatal("ожидался отказ: цель — файл, а не каталог")
		}
	})
	t.Run("создаёт отсутствующий каталог", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "вложенный", "плагин")
		res, err := Generate(plan, dir, GenerateOptions{})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if len(res.Files) != 4 {
			t.Errorf("записано %v", res.Files)
		}
	})
	t.Run("пустая папка принимается", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Generate(plan, dir, GenerateOptions{}); err != nil {
			t.Fatalf("пустая папка должна приниматься: %v", err)
		}
	})
}

// TestGenerateRefusesUnsafePluginID — id плагина попадает в манифест и в пути,
// поэтому значение извне обязано проверяться, а не «починяться» молча.
func TestGenerateRefusesUnsafePluginID(t *testing.T) {
	base := otklikPlan(t)
	unsafe := []string{
		"../побег", "a/b", `a\b`, "..", ".", "otklik.plugin", "Otklik",
		"otklik plugin", "плагин", "-otklik",
	}
	for _, id := range unsafe {
		t.Run("опции/"+id, func(t *testing.T) {
			plan := base
			plan.PluginID = "ok_id"
			if _, err := Generate(plan, filepath.Join(t.TempDir(), "d"), GenerateOptions{PluginID: id}); err == nil {
				t.Fatalf("id %q принят, а не должен", id)
			}
		})
	}
	t.Run("пустая опция берёт id из плана", func(t *testing.T) {
		// Пустой PluginID в опциях — это «не переопределяй», а не «id пустой».
		// Пустой id в самом плане — уже другая проверка, ниже.
		plan := base
		plan.PluginID = "ok_id"
		if _, err := Generate(plan, filepath.Join(t.TempDir(), "ok_id"), GenerateOptions{}); err != nil {
			t.Fatalf("ожидалось использование id из плана: %v", err)
		}
	})
	t.Run("пустой id в плане", func(t *testing.T) {
		plan := base
		plan.PluginID = ""
		if _, err := Generate(plan, filepath.Join(t.TempDir(), "d"), GenerateOptions{}); err == nil {
			t.Fatal("план без id обязан быть отвергнут")
		}
	})
	t.Run("план nil-проекта", func(t *testing.T) {
		// Plan(nil) кладёт блокер и оставляет PluginID пустым. Молча
		// подставить «imported_plugin» здесь нельзя: план без проекта —
		// это ошибка вызывающего, иGenerate обязан её показать.
		if _, err := Generate(Plan(nil), filepath.Join(t.TempDir(), "d"), GenerateOptions{}); err == nil {
			t.Fatal("ожидался отказ для Plan(nil)")
		}
	})
	t.Run("годный id из плана принимается", func(t *testing.T) {
		if _, err := Generate(base, filepath.Join(t.TempDir(), "telegram_outreach"), GenerateOptions{}); err != nil {
			t.Fatalf("годный id отвергнут: %v", err)
		}
	})
}

// TestGenerateUnresolvedNeverSilent — главное требование к честности: любая
// незапиненная зависимость обязана оставить след. Молчаливый успех здесь дал
// бы плагин, который через месяц падает на чужом железе с ImportError и не
// объясняет почему.
func TestGenerateUnresolvedNeverSilent(t *testing.T) {
	cases := []struct {
		name    string
		project *Project
	}{
		{
			name: "otklik: у всех 12 диапазон",
			project: func() *Project {
				p, err := Inspect(otklikProject(t))
				if err != nil {
					t.Fatal(err)
				}
				return p
			}(),
		},
		{
			name: "диапазон рядом с точным пином",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg-run": "pkg.api:handle"},
				Dependencies: []string{"httpx==0.27.0", "rich>=13.7,<14.0"},
			},
		},
		{
			name: "экстра-функция",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg-run": "pkg.api:handle"},
				Dependencies: []string{"sqlalchemy[asyncio]==2.0.30"},
			},
		},
		{
			name: "маркер окружения",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg-run": "pkg.api:handle"},
				Dependencies: []string{"tomli==2.0.1; python_version < \"3.11\""},
			},
		},
		{
			name: "зависимость вообще без версии",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg-run": "pkg.api:handle"},
				Dependencies: []string{"httpx"},
			},
		},
		{
			name: "прямая ссылка на источник",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg-run": "pkg.api:handle"},
				Dependencies: []string{"mypkg @ https://example.invalid/mypkg-0.1.tar.gz"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Plan(tc.project)
			if len(plan.Blockers) == 0 {
				t.Fatal("кейс должен давать блокеры плана, иначе он ничего не проверяет")
			}
			dir := filepath.Join(t.TempDir(), "pkg")
			res, err := Generate(plan, dir, GenerateOptions{})
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if !slicesContains(res.Blockers, "зависимости не запинены") {
				t.Errorf("в Blockers нет записи о незапиненных зависимостях: %v", res.Blockers)
			}
			if _, err := os.Stat(filepath.Join(dir, "requirements.lock")); err == nil {
				t.Error("requirements.lock создан, хотя записывать нечего: валидатор не поймёт, почему он неполон")
			}
			if !strings.Contains(readFile(t, filepath.Join(dir, "plugin.yaml")), "requires: []") {
				t.Error("runtime.requires не пуст: часть зависимостей записана, часть потеряна молча")
			}
			if errs := core.ValidatePluginDir(dir); len(errs) > 0 {
				t.Errorf("манифест с пустым requires невалиден: %v", errs)
			}
		})
	}
}

// TestGenerateUnresolvedImpliesBlocker — требование буквально: непустой
// Unresolved обязан дать запись в Result.Blockers. Отдельным тестом, а не
// строкой в таблице выше: тихий успех здесь — это ложь, и ловить её нужно
// точным утверждением, а не «где-то в списке есть что-то про зависимости».
func TestGenerateUnresolvedImpliesBlocker(t *testing.T) {
	plan := Plan(&Project{
		Name: "pkg", Version: "1.0", Layout: LayoutFlat,
		Scripts:      map[string]string{"pkg-run": "pkg.api:handle"},
		Dependencies: []string{"httpx==0.27.0", "rich>=13.7,<14.0", "click"},
	})
	if len(plan.Unresolved) != 2 {
		t.Fatalf("кейс должен давать два неразрешённых, ждали 2: %+v", plan.Unresolved)
	}
	res, err := Generate(plan, filepath.Join(t.TempDir(), "pkg"), GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res.Blockers) == 0 {
		t.Fatal("Unresolved непуст, а Blockers пуст — это молчаливая потеря зависимостей")
	}
	if !slicesContains(res.Blockers, "зависимости не запинены") {
		t.Fatalf("Blockers = %v", res.Blockers)
	}
	// Unresolved — подмножество Requirements, и в сообщении каждая
	// зависимость обязана встретиться один раз. Регрессия была настоящая:
	// список склеивался вдвое, и счётчик писал «24 из 12».
	locker := ""
	for _, blocker := range res.Blockers {
		if strings.Contains(blocker, "зависимости не запинены") {
			locker = blocker
		}
	}
	if !strings.Contains(locker, "не записано 2 (") {
		t.Errorf("в блокере сбит счёт: %s", locker)
	}
	for _, name := range []string{"rich", "click"} {
		if strings.Count(locker, name) != 1 {
			t.Errorf("%q в блокере %d раз(а): %s", name, strings.Count(locker, name), locker)
		}
	}
	// Частично записываемый список в runtime.requires выглядел бы готовым.
	if strings.Contains(readFile(t, filepath.Join(res.Dir, "plugin.yaml")), "httpx==0.27.0") {
		t.Error("в манифест попала одна записываемая зависимость из трёх")
	}
}

// TestGeneratePinnedDepsProduceMatchingLock — обратная сторона: когда
// записываемо ВСЁ, lock обязан появиться и совпасть с runtime.requires
// построчно. Несовпадение ловит internal/pipeline/validator.go, но только если
// lock вообще есть.
func TestGeneratePinnedDepsProduceMatchingLock(t *testing.T) {
	plan := Plan(&Project{
		Name: "Sample-Pkg", Version: "1.0", Layout: LayoutFlat,
		Scripts:      map[string]string{"sample": "sample_pkg.api:handle"},
		Dependencies: []string{"httpx==0.27.0", "rich==13.7.1"},
	})
	dir := filepath.Join(t.TempDir(), "sample_pkg")
	res, err := Generate(plan, dir, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	lock := readFile(t, filepath.Join(dir, "requirements.lock"))
	var lines []string
	for _, line := range strings.Split(lock, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if strings.Join(lines, ",") != "httpx==0.27.0,rich==13.7.1" {
		t.Fatalf("requirements.lock = %v, ждали ровно два пина по порядку", lines)
	}
	manifest := readFile(t, filepath.Join(dir, "plugin.yaml"))
	for _, want := range []string{"- httpx==0.27.0", "- rich==13.7.1"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("в runtime.requires нет %q:\n%s", want, manifest)
		}
	}
	if errs := core.ValidatePluginDir(dir); len(errs) > 0 {
		t.Fatalf("ValidatePluginDir: %v", errs)
	}
	// Записываемые зависимости — не блокер: всё, что нужно, записано.
	if slicesContains(res.Blockers, "зависимости не запинены") {
		t.Errorf("все зависимости запинены, а блокер есть: %v", res.Blockers)
	}
}

// TestGenerateManifestShape — манифест обязан лежать в схеме
// (schemas/manifest.schema.json, additionalProperties: false) и не содержать
// того, чего в схеме нет. Точки входа донора в частности: поля для них нет
// ни в схеме, ни в структуре манифеста, а KnownFields(true) отверг бы его на
// разборе.
func TestGenerateManifestShape(t *testing.T) {
	plan := otklikPlan(t)
	dir := filepath.Join(t.TempDir(), "otklik_plugin")
	if _, err := Generate(plan, dir, GenerateOptions{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	manifest := readFile(t, filepath.Join(dir, "plugin.yaml"))

	if plan.EntryPoints[0].Target != "" && strings.Contains(manifest, plan.EntryPoints[0].Target) {
		t.Errorf("точка входа донора попала в манифест:\n%s", manifest)
	}
	for _, forbidden := range []string{"entry_point", "entrypoints", "donor", "module", "sandbox"} {
		if regexp.MustCompile(`(?m)^\s*` + forbidden + `:`).MatchString(manifest) {
			t.Errorf("в манифесте есть поле %q, которого нет в схеме:\n%s", forbidden, manifest)
		}
	}
	for _, want := range []string{"id: " + plan.PluginID, "version: 0.1.0", `platform_api: "^0.1"`, "type: python", "entry: main.py"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("в манифесте нет %q:\n%s", want, manifest)
		}
	}
	// output обязан быть непустым: minProperties: 1 в схеме, плюс
	// ValidatePluginDir отдельно ругается на пустой output.
	if !strings.Contains(manifest, "output:\n") {
		t.Error("в манифесте нет output")
	}
	// Описание донора длиннее пары строк? У otklik оно короткое, но кавычки
	// и экранирование проверяются отдельным тестом ниже.
	if !strings.Contains(manifest, plan.Description) {
		t.Errorf("description не взят из метаданных проекта:\n%s", manifest)
	}
}

// TestGenerateQuotesHostileDescription — описание приходит из чужого файла.
// Кавычка, обратный слэш и перевод строки в нём сделали бы манифест нечитаемым
// или, что хуже, прочитали бы его как другой YAML.
func TestGenerateQuotesHostileDescription(t *testing.T) {
	hostile := "Плагин \"в кавычках\": да — нет\r\n\tи \\ обратный слэш"
	plan := Plan(&Project{
		Name: "pkg", Version: "1.0", Layout: LayoutFlat,
		Description:  hostile,
		Scripts:      map[string]string{"pkg-run": "pkg.api:handle"},
		Dependencies: []string{"httpx==0.27.0"},
		PackageRoot:  ".",
	})
	if plan.Description != hostile {
		t.Fatalf("Plan потерял описание: %q", plan.Description)
	}
	dir := filepath.Join(t.TempDir(), "pkg")
	if _, err := Generate(plan, dir, GenerateOptions{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if errs := core.ValidatePluginDir(dir); len(errs) > 0 {
		t.Fatalf("манифест с непристойным описанием невалиден: %v\n%s", errs, readFile(t, filepath.Join(dir, "plugin.yaml")))
	}
	m, err := core.NewEngine().LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Description != hostile {
		t.Errorf("Description = %q, ждали %q", m.Description, hostile)
	}
}

// TestGenerateMainPyIsHonestSkeleton — главное про содержимое адаптера: он
// обязан грузить функцию донора и НЕ выдумывать её вызов. Проверяется и то,
// и другое, а не только наличие файла.
func TestGenerateMainPyIsHonestSkeleton(t *testing.T) {
	root := t.TempDir()
	plan := Plan(&Project{
		Name: "pkg", Version: "1.0", Layout: LayoutFlat,
		PackageRoot: root,
		Scripts:     map[string]string{"pkg-run": "pkg.api:handle"},
	})
	if plan.ModuleImportPath != root {
		t.Fatalf("ModuleImportPath = %q, ждали %q", plan.ModuleImportPath, root)
	}
	dir := filepath.Join(t.TempDir(), "pkg")
	res, err := Generate(plan, dir, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	main := readFile(t, filepath.Join(dir, "main.py"))

	// Обратные слэши пути донора обязаны быть экранированы: без них «C:\Users»
	// в Python — это escape \U, и модуль не импортируется никогда.
	wantRoot := `DONOR_ROOT = "` + strings.ReplaceAll(root, `\`, `\\`) + `"`
	for _, want := range []string{
		wantRoot,
		`DONOR_MODULE = "pkg.api"`,
		`DONOR_ATTR = "handle"`,
		`DONOR_ENTRY = "pkg.api:handle"`,
		"donor_not_wired",
		"функцию надо выбрать вручную в редакторе плагинов",
		"TODO(человек)",
	} {
		if !strings.Contains(main, want) {
			t.Errorf("в main.py нет %q", want)
		}
	}
	// Вызова быть не должно: сигнатуру донора никто не проверял.
	if regexp.MustCompile(`(?m)^\s*donor_fn\s*\(`).MatchString(main) {
		t.Error("main.py вызывает загруженную функцию донора — этого импортёр делать не вправе")
	}
	if !slicesContains(res.Blockers, "НЕ вызывает") {
		t.Errorf("в Blockers нет признания, что вызов не написан: %v", res.Blockers)
	}
}

// TestGenerateCliOnlyEntryIsBlocker — CLI-точка входа не выбирается
// автоматически. Оборачивать `cli()` в JSON-контракт наугад нельзя: она
// читает sys.argv и возвращает код возврата.
func TestGenerateCliOnlyEntryIsBlocker(t *testing.T) {
	plan := otklikPlan(t)
	dir := filepath.Join(t.TempDir(), "otklik_plugin")
	if _, err := Generate(plan, dir, GenerateOptions{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	main := readFile(t, filepath.Join(dir, "main.py"))
	if !strings.Contains(main, `DONOR_MODULE = ""`) {
		t.Errorf("CLI-точка входа была выбрана автоматически:\n%s", firstLines(main, 60))
	}
}

// TestGenerateNoEntryPointsIsBlocker — без точек входа выбирать нечего, и это
// блокер, а не повод угадать имя функции.
func TestGenerateNoEntryPointsIsBlocker(t *testing.T) {
	plan := Plan(&Project{
		Name: "pkg", Version: "1.0", Layout: LayoutFlat,
		Scripts: map[string]string{"nothing": "broken target"},
	})
	dir := filepath.Join(t.TempDir(), "pkg")
	res, err := Generate(plan, dir, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !slicesContains(res.Blockers, "функцию надо выбрать вручную в редакторе плагинов") {
		t.Errorf("Blockers = %v", res.Blockers)
	}
	if strings.Contains(readFile(t, filepath.Join(dir, "main.py")), `DONOR_ATTR = "broken target"`) {
		t.Error("неразобранная точка входа попала в адаптер")
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "main.py")), `DONOR_MODULE = ""`) {
		t.Error("точка входа не выбрана, а константа не пустая")
	}
}

// TestGenerateReadmeListsBlockers — README обязан перечислять то же, что
// Result.Blockers, иначе «решение принято, а человеку не сказали».
func TestGenerateReadmeListsBlockers(t *testing.T) {
	plan := otklikPlan(t)
	dir := filepath.Join(t.TempDir(), "otklik_plugin")
	res, err := Generate(plan, dir, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	readme := readFile(t, filepath.Join(dir, "README.md"))
	for _, blocker := range res.Blockers {
		if !strings.Contains(readme, blocker) {
			t.Errorf("README не повторяет блокер:\n%s", blocker)
		}
	}
	for _, want := range []string{"Чего плагин НЕ умеет", "Вызова функции донора нет"} {
		if !strings.Contains(readme, want) {
			t.Errorf("в README нет %q", want)
		}
	}
	// Описание в README взято у донора — об этом обязано быть сказано, иначе
	// плагин обещает функциональность, которой в нём нет.
	if plan.Description != "" && !strings.Contains(readme, "описывает ДОНОРА, а не этот плагин") {
		t.Error("README не предупреждает, что description принадлежит донору")
	}
	// Ссылка на README донора, если он найден: это единственное место, где
	// человек узнаёт, что донор вообще делает.
	if plan.DescriptionFile != "" && !strings.Contains(readme, plan.DescriptionFile) {
		t.Errorf("README не ссылается на README донора %q", plan.DescriptionFile)
	}
}

// TestGenerateReadmePointsAtDonorReadme — README плагина ссылается на README
// донора. Иначе человек, пришедший сгенерировать плагин, не узнает, что донор
// делает, и будет читать наш адаптер вместо документации чужого проекта.
func TestGenerateReadmePointsAtDonorReadme(t *testing.T) {
	p, err := Inspect(filepath.Join("testdata", "sample"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	plan := Plan(p)
	if plan.DescriptionFile == "" {
		t.Fatal("у фикстуры sample есть README.md, а план его не нашёл")
	}
	dir := filepath.Join(t.TempDir(), plan.PluginID)
	if _, err := Generate(plan, dir, GenerateOptions{}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if readme := readFile(t, filepath.Join(dir, "README.md")); !strings.Contains(readme, plan.DescriptionFile) {
		t.Errorf("README плагина не ссылается на %q", plan.DescriptionFile)
	}
}

// TestGenerateDirNameDiffersFromIDIsWarning — каталог и id могут называться
// по-разному (ядро ищет по пути, реестр сверяет id с именем записи), но
// человек об этом должен узнать.
func TestGenerateDirNameDiffersFromIDIsWarning(t *testing.T) {
	plan := otklikPlan(t)
	dir := filepath.Join(t.TempDir(), "как_я_попросил")
	res, err := Generate(plan, dir, GenerateOptions{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !slicesContains(res.Warnings, "каталог называется") {
		t.Errorf("нет предупреждения об имени каталога: %v", res.Warnings)
	}
	// Предупреждения плана не теряются по дороге.
	if len(res.Warnings) < len(plan.Warnings) {
		t.Errorf("Generate потерял предупреждения плана: было %d, стало %d", len(plan.Warnings), len(res.Warnings))
	}
}

// TestGenerateDeterministic — два прогона на одном плане дают одинаковые
// файлы. Иначе diff двух запусков показывает изменения, которых не было.
func TestGenerateDeterministic(t *testing.T) {
	plan := otklikPlan(t)
	root := t.TempDir()
	var first map[string]string
	for _, name := range []string{"one", "two"} {
		dir := filepath.Join(root, name)
		res, err := Generate(plan, dir, GenerateOptions{})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		snapshot := map[string]string{}
		for _, f := range res.Files {
			snapshot[f] = readFile(t, filepath.Join(dir, f))
		}
		if first == nil {
			first = snapshot
			continue
		}
		if len(first) != len(snapshot) {
			t.Fatalf("разные наборы файлов: %v vs %v", first, snapshot)
		}
		for f, content := range snapshot {
			if first[f] != content {
				t.Errorf("%s отличается между прогонами", f)
			}
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
