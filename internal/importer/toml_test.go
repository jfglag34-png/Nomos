package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPyproject = `# комментарий сверху
[build-system]
requires = ["setuptools>=68", "wheel"]
build-backend = "setuptools.build_meta"

[project]
name = "telegram-outreach"
version = "0.1.0"
description = "Production-oriented Telegram outreach platform"
readme = "README.md"
requires-python = ">=3.11"
authors = [{ name = "Telegram Outreach" }]
license = { text = "MIT" }
dependencies = [
    "telethon>=1.36,<2.0",   # комментарий внутри массива
    "sqlalchemy[asyncio]>=2.0,<3.0",
]

[project.optional-dependencies]
dev = ["pytest>=8.0,<9.0"]

[project.scripts]
telegram-outreach = "telegram_outreach.main:cli"

[tool.setuptools.packages.find]
where = ["src"]
`

func TestParsePyprojectValid(t *testing.T) {
	p, err := ParsePyproject([]byte(validPyproject))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "telegram-outreach" || p.Version != "0.1.0" {
		t.Fatalf("не разобраны имя/версия: %+v", p)
	}
	if p.RequiresPython != ">=3.11" {
		t.Errorf("RequiresPython = %q", p.RequiresPython)
	}
	if len(p.Dependencies) != 2 || p.Dependencies[0] != "telethon>=1.36,<2.0" {
		t.Fatalf("зависимости разобрались как %v", p.Dependencies)
	}
	if p.OptionalDependencies["dev"][0] != "pytest>=8.0,<9.0" {
		t.Errorf("необязательная группа разобралась как %v", p.OptionalDependencies)
	}
	if p.Scripts["telegram-outreach"] != "telegram_outreach.main:cli" {
		t.Errorf("точка входа разобралась как %v", p.Scripts)
	}
	if p.Layout != LayoutSrcLayout {
		t.Errorf("Layout = %q, а where = [\"src\"]", p.Layout)
	}
	if p.readmeRef != "README.md" {
		t.Errorf("readmeRef = %q", p.readmeRef)
	}
}

// TestParsePyprojectRefuses — главный тест пакета: всё непонятное обязано
// давать ошибку с номером строки. Тихий пропуск здесь означал бы потерю
// зависимости, о которой никто не узнает.
func TestParsePyprojectRefuses(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "битый TOML: версия без кавычек",
			src:     "[project]\nname = \"pkg\"\nversion = 0.1.0\n",
			wantErr: "строка 3",
		},
		{
			name:    "битый TOML: незакрытая строка",
			src:     "[project]\nname = \"pkg\nversion = \"1.0\"\n",
			wantErr: "строка 2",
		},
		{
			name:    "битый TOML: незакрытый массив",
			src:     "[project]\nname = \"pkg\"\ndependencies = [\"httpx==1.0\"\n",
			wantErr: "массив не закрыт",
		},
		{
			name:    "битый TOML: значение без '='",
			src:     "[project]\nname \"pkg\"\n",
			wantErr: "жду '='",
		},
		{
			name:    "битый TOML: мусор после значения",
			src:     "[project]\nname = \"pkg\" version = \"1.0\"\n",
			wantErr: "после значения",
		},
		{
			name:    "неизвестная конструкция: массив таблиц",
			src:     "[project]\nname = \"pkg\"\n\n[[tool.poetry.packages]]\nname = \"pkg\"\n",
			wantErr: "массив таблиц [[..]] не поддерживается",
		},
		{
			name:    "неизвестная конструкция: дата-время",
			src:     "[project]\nname = \"pkg\"\nrelease = 2024-05-27T07:32:00Z\n",
			wantErr: "строка 3",
		},
		{
			name:    "неизвестная конструкция: не-ASCII имя ключа",
			src:     "[project]\nимя = \"pkg\"\n",
			wantErr: "ожидался ключ",
		},
		{
			name:    "неизвестная конструкция: перенос строки внутри строки",
			src:     "[project]\nname = \"pkg\"\ndescription = \"первая\nвторая\"\n",
			wantErr: "необработанный перевод строки",
		},
		{
			name:    "неизвестная конструкция: неизвестный escape",
			src:     "[project]\nname = \"pkg\"\ndescription = \"a\\qb\"\n",
			wantErr: "неизвестная escape-последовательность",
		},
		{
			name:    "дубликат ключа",
			src:     "[project]\nname = \"pkg\"\nname = \"other\"\n",
			wantErr: "дубликат",
		},
		{
			name:    "секция объявлена повторно",
			src:     "[project]\nname = \"pkg\"\n\n[project]\nversion = \"1.0\"\n",
			wantErr: "уже объявлена",
		},
		{
			name:    "нет секции [project]",
			src:     "[tool.poetry]\nname = \"pkg\"\n\n[build-system]\nrequires = []\n",
			wantErr: "нет секции [project]",
		},
		{
			name:    "в [project] нет name",
			src:     "[project]\nversion = \"1.0\"\n",
			wantErr: "нет name",
		},
		{
			name:    "dependencies не строка, а число",
			src:     "[project]\nname = \"pkg\"\ndependencies = 5\n",
			wantErr: "массив строк",
		},
		{
			name:    "точка входа не строка",
			src:     "[project]\nname = \"pkg\"\n\n[project.scripts]\npkg = 5\n",
			wantErr: "должна быть строкой",
		},
		{
			name:    "точка входа — таблица",
			src:     "[project]\nname = \"pkg\"\n\n[project.scripts.pkg]\nmod = \"a.b\"\n",
			wantErr: "должна быть строкой",
		},
		{
			name:    "группа optional-dependencies — не массив",
			src:     "[project]\nname = \"pkg\"\n\n[project.optional-dependencies]\ndev = \"pytest\"\n",
			wantErr: "массив строк",
		},
		{
			name:    "пустой файл",
			src:     "",
			wantErr: "нет секции [project]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := ParsePyproject([]byte(tc.src))
			if err == nil {
				t.Fatalf("ожидалась ошибка %q, а разбор прошёл: %+v", tc.wantErr, p)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ошибка %q не содержит %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestParsePyprojectKeepsEverythingItUnderstood — проверка на то, что «мы всё
// разобрали»: значения не теряются по дороге, включая числа, булевы и
// встроенные таблицы, которые в реальных pyproject встречаются постоянно.
func TestParsePyprojectKeepsEverythingItUnderstood(t *testing.T) {
	src := "[project]\nname = \"pkg\"\nversion = 1  # число, а не строка\n" +
		"[tool.demo]\nflag = true\ncount = 42\nratio = 0.5\n" +
		"inline = { a = \"b\", c = [1, 2] }\nmultiline = \"\"\"первая\nвторая\"\"\"\n"
	doc, err := parseToml(src)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePyproject([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "1" {
		t.Errorf("Version = %q, ждали текст числа \"1\"", p.Version)
	}
	demo, ok, err := doc.root.table("tool")
	if err != nil || !ok {
		t.Fatalf("секция [tool] не найдена: ok=%v err=%v", ok, err)
	}
	demoNode, ok, err := demo.table("demo")
	if err != nil || !ok {
		t.Fatalf("секция [tool.demo] не найдена: ok=%v err=%v", ok, err)
	}
	for _, name := range []string{"flag", "count", "ratio", "inline", "multiline"} {
		if _, ok := demoNode.fields[name]; !ok {
			t.Errorf("ключ %q потерялся при разборе", name)
		}
	}
	// Булево значение на месте, где ждали строку, — отказ, а не тихая
	// подстановка: превращать true в "true" значило бы выдумать метаданные.
	if _, _, err := demoNode.stringField("flag"); err == nil {
		t.Error("stringField принял булево значение — ждали отказ")
	}
	if kind := demoNode.fields["flag"].val.kind; kind != tomlBool {
		t.Errorf("kind флага = %v, ждали tomlBool", kind)
	}
	if v, ok, err := demoNode.stringField("multiline"); err != nil || !ok || v != "первая\nвторая" {
		t.Errorf("multiline = %q ok=%v err=%v", v, ok, err)
	}
}

// TestParseTomlNumbers — числа в TOML нужны версии и счётчикам, а форма
// `_`-разделителей и шестнадцатеричной записи встречается в настройках.
// Текст сохраняется дословно, чтобы версия не потеряла символы.
func TestParseTomlNumbers(t *testing.T) {
	doc, err := parseToml("[x]\na = 1_000\nb = 0x1F\nc = 0o17\nd = 1.5e3\ne = 12\n")
	if err != nil {
		t.Fatal(err)
	}
	x, ok, err := doc.root.table("x")
	if err != nil || !ok {
		t.Fatalf("[x]: ok=%v err=%v", ok, err)
	}
	for name, want := range map[string]string{
		"a": "1_000", "b": "0x1F", "c": "0o17", "d": "1.5e3", "e": "12",
	} {
		got, ok, err := x.stringField(name)
		if err != nil || !ok || got != want {
			t.Errorf("%s = %q (ok=%v err=%v), ждали %q", name, got, ok, err, want)
		}
	}
}

// TestParseTomlCRLFAndComments — файл может прийти с CRLF и с комментариями в
// конце строки: и то, и другое не должно выглядеть как «после значения мусор».
func TestParseTomlCRLFAndComments(t *testing.T) {
	src := "[project]   # секция\r\nname = \"pkg\"  # имя\r\ndependencies = [\r\n  \"httpx==1.0\",\r\n]\r\n"
	p, err := ParsePyproject([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "pkg" || len(p.Dependencies) != 1 {
		t.Fatalf("CRLF-файл разобрался как %+v", p)
	}
}

// TestParseTomlDottedKeysAndMultilineStrings — фиксирует возможности, о которых
// написано в докомментации к parseToml: точечные ключи вне заголовка и
// многострочные строки обоих видов. Если поддержка исчезнет, тест упадёт
// раньше, чем на неё напорется чужой pyproject.
func TestParseTomlDottedKeysAndMultilineStrings(t *testing.T) {
	src := "[project]\nname = \"pkg\"\ndescription = \"\"\"строка\nв две строки\"\"\"\n" +
		"literal = '''буквально\nвсё как есть'''\n"
	doc, err := parseToml(src)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePyproject([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if p.Description != "строка\nв две строки" {
		t.Errorf("Description = %q", p.Description)
	}
	// Точечный ключ относится к той таблице, в которой он написан, поэтому
	// проверяется отдельно и без заголовков перед ним.
	dotted, err := parseToml("tool.ruff.line-length = 100\n")
	if err != nil {
		t.Fatal(err)
	}
	tool, ok, err := dotted.root.table("tool")
	if err != nil || !ok {
		t.Fatalf("[tool] не найдена: ok=%v err=%v", ok, err)
	}
	ruff, ok, err := tool.table("ruff")
	if err != nil || !ok {
		t.Fatalf("[tool.ruff] не найдена: ok=%v err=%v", ok, err)
	}
	if v, ok, err := ruff.stringField("line-length"); err != nil || !ok || v != "100" {
		t.Errorf("точечный ключ дал %q ok=%v err=%v", v, ok, err)
	}
	project, _, _ := doc.root.table("project")
	if v, _, err := project.stringField("literal"); err != nil || v != "буквально\nвсё как есть" {
		t.Errorf("буквальная многострочная строка дала %q err=%v", v, err)
	}
	// Переоткрытие таблицы после точечного ключа — легальный TOML
	// (`tool.ruff.line-length = 100`, потом `[tool.ruff]`), и он не должен
	// выглядеть как дубликат.
	if _, err := parseToml("tool.ruff.line-length = 100\n\n[tool.ruff]\nselect = [\"E\"]\n"); err != nil {
		t.Fatalf("переоткрытие таблицы после точечного ключа: %v", err)
	}
}

// TestParsePyprojectOtklikFixture — критерий приёмки на реальном файле из
// https://github.com/wykserdex/otklik (копия лежит в testdata/otklik_pyproject.toml).
//
// Число зависимостей здесь 12, а не 13: в файле их ровно 12 (строки 14–25),
// проверено пересчётом, и тест привязан к факту. Дописать тринадцатую
// «чтобы сошлось с ожиданием» означало бы врать о реальном проекте.
func TestParsePyprojectOtklikFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "otklik_pyproject.toml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParsePyproject(raw)
	if err != nil {
		t.Fatalf("реальный pyproject.toml otklik не разобрался: %v", err)
	}
	if p.Name != "telegram-outreach" {
		t.Errorf("Name = %q, ждали telegram-outreach", p.Name)
	}
	if p.Version != "0.1.0" {
		t.Errorf("Version = %q, ждали 0.1.0", p.Version)
	}
	if p.RequiresPython != ">=3.11" {
		t.Errorf("RequiresPython = %q, ждали >=3.11", p.RequiresPython)
	}
	wantDeps := []string{
		"telethon>=1.36,<2.0", "sqlalchemy[asyncio]>=2.0,<3.0", "alembic>=1.13,<2.0",
		"asyncpg>=0.29,<1.0", "pydantic>=2.6,<3.0", "pydantic-settings>=2.2,<3.0",
		"httpx>=0.27,<1.0", "structlog>=24.1,<25.0", "python-dotenv>=1.0,<2.0",
		"tenacity>=8.2,<9.0", "click>=8.1,<9.0", "rich>=13.7,<14.0",
	}
	if len(p.Dependencies) != len(wantDeps) {
		t.Fatalf("зависимостей %d, ждали %d: %v", len(p.Dependencies), len(wantDeps), p.Dependencies)
	}
	for i, want := range wantDeps {
		if p.Dependencies[i] != want {
			t.Errorf("зависимость %d = %q, ждали %q", i, p.Dependencies[i], want)
		}
	}
	if len(p.Scripts) != 1 {
		t.Fatalf("точек входа %d, ждали 1: %v", len(p.Scripts), p.Scripts)
	}
	if p.Scripts["telegram-outreach"] != "telegram_outreach.main:cli" {
		t.Errorf("точка входа = %q, ждали telegram_outreach.main:cli", p.Scripts["telegram-outreach"])
	}
	if p.Layout != LayoutSrcLayout {
		t.Errorf("Layout = %q, ждали %s (where = [\"src\"])", p.Layout, LayoutSrcLayout)
	}

	plan := Plan(p)
	if plan.PluginID != "telegram_outreach" {
		t.Errorf("PluginID = %q, ждали telegram_outreach", plan.PluginID)
	}
	if len(plan.Requirements) != len(wantDeps) {
		t.Fatalf("в плане %d требований, ждали %d", len(plan.Requirements), len(wantDeps))
	}
	if len(plan.Unresolved) != len(wantDeps) {
		t.Fatalf("неразрешённых %d, ждали %d: %v", len(plan.Unresolved), len(wantDeps), plan.Unresolved)
	}
	// Ключевое: у всех 12 версии заданы ДИАПАЗОНОМ, ни у одной нет `==`.
	// Если бы импортёр умел «дописать» версию, эти строки поехали бы в
	// манифест как выдуманные пины.
	for _, req := range plan.Unresolved {
		if req.IsPinned || req.Version != "" {
			t.Errorf("%q: версия выдумана (Version=%q IsPinned=%v)", req.Raw, req.Version, req.IsPinned)
		}
		if _, ok := req.ManifestSpec(); ok {
			t.Errorf("%q: ManifestSpec дал строку для манифеста без точного пина", req.Raw)
		}
	}
	if plan.ModuleImportPath != "src" {
		t.Errorf("ModuleImportPath = %q, ждали src (PackageRoot не задан, Plan вызван без Inspect)", plan.ModuleImportPath)
	}
	// 12 блокеров на зависимости плюс один на CLI-подобный вход
	if len(plan.Blockers) != len(wantDeps)+1 {
		t.Errorf("блокеров %d, ждали %d: %v", len(plan.Blockers), len(wantDeps)+1, plan.Blockers)
	}

	// Необязательная группа подключается только явно.
	withDev := Plan(&Project{
		Name:                 p.Name,
		Version:              p.Version,
		Dependencies:         p.Dependencies,
		OptionalDependencies: p.OptionalDependencies,
		Scripts:              p.Scripts,
		Layout:               p.Layout,
		OptionalGroup:        "dev",
	})
	if len(withDev.Requirements) != 20 {
		t.Errorf("с группой dev требований %d, ждали 20 (12 основных + 8 из dev)", len(withDev.Requirements))
	}
}

// TestPlanAcceptsNothingOnItsOwn — план без Inspect честно говорит, что путь
// к модулям неполный, вместо того чтобы выдумать абсолютный путь.
func TestPlanAcceptsNothingOnItsOwn(t *testing.T) {
	p := &Project{Name: "9lives", Scripts: map[string]string{"9lives-run": "nine.api:handle"}}
	plan := Plan(p)
	if plan.PluginID != "p_9lives" {
		t.Errorf("PluginID = %q, ждали p_9lives (id не может начинаться с цифры)", plan.PluginID)
	}
	if len(plan.Blockers) == 0 {
		t.Error("без точек входа и без раскладки план обязан быть полон блокеров")
	}
}
