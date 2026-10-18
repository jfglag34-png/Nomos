package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInspectSrcLayout — раскладка src-layout на настоящем каталоге: пакет
// лежит в src/, и именно этот каталог обязан попасть в ModuleImportPath.
// Ошибка здесь означала бы, что плагин не найдёт модули чужого проекта.
func TestInspectSrcLayout(t *testing.T) {
	dir := filepath.Join("testdata", "sample")
	p, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "sample-pkg" || p.Version != "1.4.2" {
		t.Fatalf("метаданные разобрались как %+v", p)
	}
	if p.RequiresPython != ">=3.10" {
		t.Errorf("RequiresPython = %q", p.RequiresPython)
	}
	if len(p.Dependencies) != 2 {
		t.Fatalf("зависимостей %d: %v", len(p.Dependencies), p.Dependencies)
	}
	if p.Layout != LayoutSrcLayout {
		t.Errorf("Layout = %q, ждали %s", p.Layout, LayoutSrcLayout)
	}
	if p.PackageRoot == "" || filepath.Base(p.PackageRoot) != "sample" {
		t.Errorf("PackageRoot = %q", p.PackageRoot)
	}
	wantDescription := filepath.Join(dir, "README.md")
	if p.DescriptionFile != wantDescription {
		t.Errorf("DescriptionFile = %q, ждали %q", p.DescriptionFile, wantDescription)
	}

	plan := Plan(p)
	if plan.PluginID != "sample_pkg" {
		t.Errorf("PluginID = %q, ждали sample_pkg", plan.PluginID)
	}
	if plan.ModuleImportPath != filepath.Join(dir, "src") {
		t.Errorf("ModuleImportPath = %q, ждали %q", plan.ModuleImportPath, filepath.Join(dir, "src"))
	}
	if len(plan.EntryPoints) != 1 || plan.EntryPoints[0].Target != "sample_pkg.main:parse_cli" {
		t.Fatalf("точки входа разобрались как %+v", plan.EntryPoints)
	}
	// Точный пин httpx==0.27.0 записывается в манифест как есть, диапазон
	// rich — попадает в Unresolved и в блокеры.
	if len(plan.Unresolved) != 1 || plan.Unresolved[0].Name != "rich" {
		t.Errorf("неразрешённые = %+v, ждали только rich", plan.Unresolved)
	}
	spec, ok := plan.Requirements[0].ManifestSpec()
	if !ok || spec != "httpx==0.27.0" {
		t.Errorf("ManifestSpec = %q ok=%v, ждали httpx==0.27.0", spec, ok)
	}
	// Эвристика «похоже на CLI» намеренно узкая (список имён, а не «вхождение
	// подстроки cli»: иначе под неё попадает `client`). Поэтому `parse_cli`
	// блокера не даёт — и это проверяется, чтобы однажды расширение списка
	// не сделалось молча и незаметно.
	if hasBlockerContaining(plan.Blockers, "похожи на CLI") {
		t.Errorf("parse_cli не должен распознаваться как CLI-вход: %v", plan.Blockers)
	}
	if !hasWarningContaining(plan.Warnings, "README") && p.DescriptionFile == "" {
		t.Error("README найден, предупреждение не нужно")
	}
}

// TestInspectFlatLayout — раскладка flat: пакет в корне, никакого src/ нет.
// ModuleImportPath тогда — сам корень проекта.
func TestInspectFlatLayout(t *testing.T) {
	dir := filepath.Join("testdata", "flat")
	p, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Layout != LayoutFlat {
		t.Fatalf("Layout = %q, ждали %s", p.Layout, LayoutFlat)
	}
	plan := Plan(p)
	if plan.ModuleImportPath == "" {
		t.Error("для flat-проекта ModuleImportPath обязан быть корнем проекта")
	}
	if p.DescriptionFile != "" {
		t.Errorf("DescriptionFile = %q, а README в проекте нет", p.DescriptionFile)
	}
	if !hasWarningContaining(plan.Warnings, "README не найден") {
		t.Errorf("об отсутствии README нет предупреждения: %v", plan.Warnings)
	}
}

// TestInspectSetupCfgFallback — запасной путь через setup.cfg. Он существует
// не для симметрии, а потому что проекты на setuptools до сих пор так
// поставляются, а pyproject.toml у них может не быть вовсе.
func TestInspectSetupCfgFallback(t *testing.T) {
	dir := filepath.Join("testdata", "setupcfg")
	p, err := Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "cfg-only-tool" || p.Version != "0.9.1" {
		t.Fatalf("метаданные разобрались как %+v", p)
	}
	if p.RequiresPython != ">=3.9" {
		t.Errorf("RequiresPython = %q", p.RequiresPython)
	}
	if len(p.Dependencies) != 2 || p.Dependencies[0] != "requests>=2.31,<3.0" {
		t.Fatalf("install_requires разобрался как %v", p.Dependencies)
	}
	if p.Scripts["cfg-tool"] != "cfg_only.main:main_cli" {
		t.Errorf("console_scripts разобрался как %v", p.Scripts)
	}
	if p.SourceFile() != "setup.cfg" {
		t.Errorf("SourceFile = %q, ждали setup.cfg", p.SourceFile())
	}
	if p.Layout != LayoutSrcLayout {
		t.Errorf("Layout = %q, а where = src", p.Layout)
	}
	if len(p.OptionalDependencies["dev"]) != 2 {
		t.Errorf("extras_require разобрался как %v", p.OptionalDependencies)
	}
	if p.DescriptionFile == "" {
		t.Error("README из long_description = file: README.md не найден")
	}
}

// TestInspectRefusesSetupPy — главное осознанное решение пакета. setup.py —
// исполняемый код чужого автора; запускать его при импорте нельзя, поэтому
// проект без pyproject.toml и setup.cfg отвергается, даже если setup.py
// читается глазами за секунду.
func TestInspectRefusesSetupPy(t *testing.T) {
	dir := filepath.Join("testdata", "setuppy")
	p, err := Inspect(dir)
	if err == nil {
		t.Fatalf("ожидался отказ по setup.py, а вернулось %+v", p)
	}
	if !strings.Contains(err.Error(), "setup.py это исполняемый код чужого автора") {
		t.Errorf("сообщение не объясняет причину: %v", err)
	}
	if !strings.Contains(err.Error(), "pyproject.toml или setup.cfg") {
		t.Errorf("сообщение не подсказывает, что делать: %v", err)
	}
}

// TestInspectRefusals — остальные отказы Inspect: пустой путь, не каталог,
// каталог без метаданных, сломанный файл. Все они обязаны называть причину.
func TestInspectRefusals(t *testing.T) {
	t.Run("пустой путь", func(t *testing.T) {
		if _, err := Inspect("  "); err == nil {
			t.Fatal("ожидался отказ")
		}
	})
	t.Run("не каталог", func(t *testing.T) {
		if _, err := Inspect(filepath.Join("testdata", "otklik_pyproject.toml")); err == nil {
			t.Fatal("ожидался отказ")
		}
	})
	t.Run("нет метаданных", func(t *testing.T) {
		if _, err := Inspect(t.TempDir()); err == nil {
			t.Fatal("ожидался отказ")
		}
	})
	t.Run("битый pyproject", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"),
			[]byte("[project]\nname = \"pkg\"\nversion = 0.1.0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Inspect(dir)
		if err == nil || !strings.Contains(err.Error(), "строка 3") {
			t.Fatalf("битый файл должен дать отказ с номером строки, получили %v", err)
		}
	})
	t.Run("битый setup.cfg", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "setup.cfg"),
			[]byte("[metadata]\nname = \"pkg\"\nversion 1.0\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(dir); err == nil {
			t.Fatal("ожидался отказ на битом setup.cfg")
		}
	})
	t.Run("pyproject важнее setup.cfg", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"),
			[]byte("[project]\nname = \"from-pyproject\"\nversion = \"1.0\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "setup.cfg"),
			[]byte("[metadata]\nname = \"from-setup-cfg\"\nversion = \"2.0\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		p, err := Inspect(dir)
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != "from-pyproject" {
			t.Errorf("Name = %q, ждали from-pyproject (PEP 621 — источник истины)", p.Name)
		}
		if !hasWarningContaining(Plan(p).Warnings, "setup.cfg") {
			t.Errorf("проигнорированный setup.cfg не отмечен: %v", Plan(p).Warnings)
		}
	})
}

// TestPlanBlockers — планы на руках: сценарии, где автоматический импорт
// невозможен, и импортёр обязан сказать об этом, а не сделать вид, что всё
// в порядке.
func TestPlanBlockers(t *testing.T) {
	tests := []struct {
		name      string
		project   *Project
		wantBlock []string
		wantWarn  []string
	}{
		{
			name: "нет точек входа",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Dependencies: []string{"httpx==1.0"},
			},
			wantBlock: []string{"точек входа нет"},
		},
		{
			name: "зависимость без версии",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg": "pkg.api:handle"},
				Dependencies: []string{"httpx"},
			},
			wantBlock: []string{"объявлен без версии вообще"},
		},
		{
			name: "экстра-функция не представима в манифесте",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg": "pkg.api:handle"},
				Dependencies: []string{"sqlalchemy[asyncio]==2.0.30"},
			},
			wantBlock: []string{"экстра-функцию в манифесте выразить нечем"},
		},
		{
			name: "маркер окружения",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg": "pkg.api:handle"},
				Dependencies: []string{"tomli==2.0.1; python_version < \"3.11\""},
			},
			wantBlock: []string{"маркер окружения"},
		},
		{
			name: "прямая ссылка на источник",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg": "pkg.api:handle"},
				Dependencies: []string{"mypkg @ https://example.invalid/mypkg-0.1.tar.gz"},
			},
			wantBlock: []string{"прямой ссылкой"},
		},
		{
			name: "мусор в зависимости не теряется",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:      map[string]string{"pkg": "pkg.api:handle"},
				Dependencies: []string{"httpx==1.0", "-r base.txt"},
			},
			wantBlock: []string{"не разобралась"},
		},
		{
			name: "точка входа без модуля",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts: map[string]string{"pkg": "handle"},
			},
			wantBlock: []string{"не похоже на \"модуль:функция\""},
		},
		{
			name: "все входы похожи на CLI",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts: map[string]string{"pkg-cli": "pkg.app:cli", "pkg-serve": "pkg.server:serve"},
			},
			wantBlock: []string{"похожи на CLI"},
		},
		{
			name: "часть входов похожа на CLI",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts: map[string]string{"pkg-handle": "pkg.api:handle", "pkg-cli": "pkg.app:cli"},
			},
			wantWarn: []string{"часть точек входа похожа на CLI"},
		},
		{
			name: "раскладка неизвестна",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutUnknown,
				Scripts: map[string]string{"pkg-handle": "pkg.api:handle"},
			},
			wantBlock: []string{"раскладка исходников неизвестна"},
		},
		{
			name: "группа объявлена, её нет",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:              map[string]string{"pkg-handle": "pkg.api:handle"},
				OptionalGroup:        "gpu",
				OptionalDependencies: map[string][]string{"dev": {"pytest"}},
			},
			wantBlock: []string{"не заявлена в проекте"},
		},
		{
			name: "версия не объявлена",
			project: &Project{
				Name: "pkg", Layout: LayoutFlat,
				Scripts: map[string]string{"pkg-handle": "pkg.api:handle"},
			},
			wantWarn: []string{"версия проекта не объявлена"},
		},
		{
			name: "дубликат зависимости",
			project: &Project{
				Name: "pkg", Version: "1.0", Layout: LayoutFlat,
				Scripts:              map[string]string{"pkg-handle": "pkg.api:handle"},
				Dependencies:         []string{"httpx==1.0"},
				OptionalDependencies: map[string][]string{"dev": {"HTTPX==2.0"}},
				OptionalGroup:        "dev",
			},
			wantWarn: []string{"повторяется"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan := Plan(tc.project)
			for _, want := range tc.wantBlock {
				if !hasBlockerContaining(plan.Blockers, want) {
					t.Errorf("нет блокера с %q; блокеры: %v", want, plan.Blockers)
				}
			}
			for _, want := range tc.wantWarn {
				if !hasWarningContaining(plan.Warnings, want) {
					t.Errorf("нет предупреждения с %q; предупреждения: %v", want, plan.Warnings)
				}
			}
		})
	}
}

// TestPlanNilProject — nil не должен ронять импортёр молча в панику: план
// возвращается с блокером, потому что вызывающий код обычно печатает план, а
// не проверяет ошибку.
func TestPlanNilProject(t *testing.T) {
	plan := Plan(nil)
	if !hasBlockerContaining(plan.Blockers, "nil") {
		t.Errorf("ожидался блокер про nil, получили %v", plan.Blockers)
	}
}

// TestPlanEntryPointsOrder — порядок точек входа детерминированный: два
// запуска на одном проекте должны давать одинаковый вывод, иначе diff двух
// запусков показывает изменения, которых не было.
func TestPlanEntryPointsOrder(t *testing.T) {
	p := &Project{
		Name: "pkg", Version: "1.0", Layout: LayoutFlat,
		Scripts: map[string]string{
			"zebra": "pkg.z:handle", "alpha": "pkg.a:handle", "middle": "pkg.m:handle",
		},
	}
	first := Plan(p)
	second := Plan(p)
	if len(first.EntryPoints) != 3 {
		t.Fatalf("точек входа %d: %+v", len(first.EntryPoints), first.EntryPoints)
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, ep := range first.EntryPoints {
		if ep.Name != want[i] {
			t.Errorf("entry[%d] = %q, ждали %q", i, ep.Name, want[i])
		}
	}
	if strings.Join(targetList(first.EntryPoints), ",") != strings.Join(targetList(second.EntryPoints), ",") {
		t.Error("порядок точек входа нестабилен между вызовами")
	}
}

// TestSanitizePluginID — id плагина обязан пройти правило
// internal/core/plugincreate.go, иначе импорт «пройдёт», а создание
// плагина потом отвергнет id.
func TestSanitizePluginID(t *testing.T) {
	cases := map[string]string{
		"telegram-outreach": "telegram_outreach",
		"Click":             "click",
		"zope.interface":    "zope_interface",
		"a--b":              "a_b",
		"--leading":         "leading",
		"9lives":            "p_9lives",
		"проект":            "",
		"":                  "",
	}
	for input, want := range cases {
		got := sanitizePluginID(input)
		if got != want {
			t.Errorf("sanitizePluginID(%q) = %q, ждали %q", input, got, want)
		}
		if got != "" && !pluginIDRe.MatchString(got) {
			t.Errorf("sanitizePluginID(%q) = %q не проходит ^[a-z][a-z0-9_]*$", input, got)
		}
	}
	// Пустое имя должно всё равно давать годный id, а не пустую строку.
	if plan := Plan(&Project{Name: "проект"}); !pluginIDRe.MatchString(plan.PluginID) {
		t.Errorf("Plan дал негодный PluginID %q", plan.PluginID)
	}
}

func hasBlockerContaining(blockers []string, want string) bool {
	return slicesContains(blockers, want)
}

func hasWarningContaining(warnings []string, want string) bool {
	return slicesContains(warnings, want)
}

func slicesContains(list []string, want string) bool {
	for _, item := range list {
		if strings.Contains(item, want) {
			return true
		}
	}
	return false
}
