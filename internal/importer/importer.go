// Package importer читает метаданные ЧУЖОГО Python-проекта и превращает их в
// план импорта плагина WEDRA. Первый слой: он ничего не пишет на диск, только
// анализирует и возвращает план.
//
// Второй слой — Generate (generate.go): по ImportPlan он создаёт на диске
// каталог плагина. Скелет, а не работающий плагин: вызов функции донора
// импортёр написать не вправе (сигнатуру он не проверял), и всё нерешённое
// уходит в Result.Blockers и в README, а не растворяется в файлах.
//
// Главное правило пакета — враньё вслух. Импортёр читает файл, написанный
// не нами, и строит по нему манифест с жёсткой схемой. Всё, что не понято,
// должно приводить к отказу с номером строки, а не к тихому пропуску:
// пропущенная зависимость даёт плагин, который падает на чужом железе через
// месяц и не объясняет почему.
//
// Три решения, которые здесь приняты сознательно и которые выглядят как
// недоработка:
//
//  1. setup.py НЕ выполняется и НЕ разбирается. Это исполняемый код
//     незнакомого автора: он может скачать что угодно из сети, а мы бы это
//     запустили от имени пользователя, который просто указал путь. Поэтому
//     проект без pyproject.toml и setup.cfg — отказ, даже если рядом лежит
//     полностью читаемый setup.py. Читать setup.py как текст тоже нельзя:
//     метаданные там считаются кодом, и «прочитанный на глаз» setup.py даст
//     правдоподобную, но выдуманную версию.
//
//  2. Диапазон версий НЕ превращается в пин. `runtime.requires` по схеме
//     (schemas/manifest.schema.json) допускает ровно `package==version`, и
//     `requirements.lock` обязан содержать ровно те же строки
//     (internal/pipeline/validator.go). Из `>=1.36,<2.0` нельзя получить одну
//     версию, не заглянув в чужое окружение; выбрать «последнюю подходящую»
//     значит записать в манифест догадку как факт. Такие зависимости попадают
//     в Unresolved и в Blockers — их надо сначала разрешить (pip freeze в
//     окружении проекта), и это отдельный шаг, а не флаг.
//
//  3. Никаких сетевых вызовов и никакого exec. Даже `python -c`, даже
//     `pip index`. Пакет работает на том, что человек уже скачал, и ничего не
//     выполняет: единственный исполняемый артефакт в репозитории — сам бинарь
//     WEDRA, а импортёр к его исполнению отношения не имеет.
package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Значения Layout. Константы, а не свободные строки: значение уходит в
// сообщения и в решения, и «src_layout» вместо «src-layout» никто не заметит
// в тесте, а человек увидит в выводе.
const (
	LayoutSrcLayout = "src-layout"
	LayoutFlat      = "flat"
	LayoutUnknown   = "unknown"
)

// Project — метаданные чужого проекта ровно в том виде, в каком они объявлены.
type Project struct {
	// Name — имя дистрибутива как в файле ("telegram-outreach"), без
	// нормализации: человек должен видеть то, что написано у него.
	Name string
	// Version — версия проекта.
	Version string
	// Description — однострочное описание, если объявлено.
	Description string
	// RequiresPython — requires-python / python_requires, исходным текстом
	// (">=3.11"). Диапазон, а не версия: интерпретатор плагина мы не
	// проверяем и не подбираем.
	RequiresPython string
	// Dependencies — основные зависимости, исходными строками.
	Dependencies []string
	// OptionalDependencies — необязательные группы (в pyproject это
	// [project.optional-dependencies], в setup.cfg — `extras_require`).
	OptionalDependencies map[string][]string
	// Scripts — точки входа из [project.scripts]: имя команды -> "модуль:функция".
	Scripts map[string]string
	// PackageRoot — каталог проекта (тот, где лежит pyproject.toml/setup.cfg).
	PackageRoot string
	// Layout — раскладка исходников: LayoutSrcLayout, LayoutFlat или
	// LayoutUnknown. По ней считается ModuleImportPath в плане.
	Layout string
	// DescriptionFile — путь к найденному README (абсолютный). Это описание
	// проекта, а не плагина: в манифест его дословно класть нельзя.
	DescriptionFile string

	// OptionalGroup — необязательная группа, которую решили включить в импорт.
	// Пусто означает «только основные зависимости»: молча включать все
	// группы нельзя, там тестовые и dev-инструменты.
	OptionalGroup string

	// Ниже — то, что видно только при чтении файла, и что не имеет смысла
	// вне контекста разбора.

	// readmeRef — что объявлено в [project] readme (путь, как в файле).
	readmeRef string
	// dynamic — поля, отданные бэкенду сборки ([project] dynamic).
	dynamic []string
	// findWhere — значение [tool.setuptools.packages.find] where: подсказка
	// о раскладке, которую файл заявляет сам.
	findWhere string
	// notes — предупреждения, родившиеся при чтении файла. План переносит их
	// в Warnings, чтобы решение человека было при плане, а не после него.
	notes []string
	// sourceFile — откуда взяты метаданные: "pyproject.toml" или "setup.cfg".
	sourceFile string
}

// SourceFile — имя файла, из которого взяты метаданные. Полезно в сообщениях:
// у setup.cfg и pyproject.toml разные слепые зоны, и человек должен знать, что
// именно читалось.
func (p *Project) SourceFile() string {
	if p == nil {
		return ""
	}
	return p.sourceFile
}

// dynamicField сообщает, объявлено ли поле как отданное сборочному бэкенду.
func (p *Project) dynamicField(name string) bool {
	for _, item := range p.dynamic {
		if item == name {
			return true
		}
	}
	return false
}

// Inspect читает метаданные проекта в каталоге. Порядок источников задан один
// раз и не «как попалось»:
//
//	pyproject.toml → setup.cfg → отказ со ссылкой на setup.py.
//
// pyproject важнее setup.cfg даже если оба лежат рядом: PEP 621 — источник
// истины, а setup.cfg в таком проекте часто остался от старой сборки и
// расходится с ним. Расхождение — повод для предупреждения, а не для выбора
// «более нового» файла.
func Inspect(dir string) (*Project, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("каталог не указан")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("каталог %s недоступен: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s — не каталог", dir)
	}

	var p *Project
	switch {
	case fileExists(filepath.Join(dir, "pyproject.toml")):
		raw, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
		if err != nil {
			return nil, fmt.Errorf("не читается pyproject.toml: %w", err)
		}
		if p, err = ParsePyproject(raw); err != nil {
			return nil, err
		}
		if fileExists(filepath.Join(dir, "setup.cfg")) {
			p.notes = append(p.notes,
				"рядом лежит setup.cfg — он проигнорирован, метаданные взяты из pyproject.toml (PEP 621)")
		}
	case fileExists(filepath.Join(dir, "setup.cfg")):
		raw, err := os.ReadFile(filepath.Join(dir, "setup.cfg"))
		if err != nil {
			return nil, fmt.Errorf("не читается setup.cfg: %w", err)
		}
		if p, err = parseSetupCfg(raw); err != nil {
			return nil, fmt.Errorf("setup.cfg: %w", err)
		}
	case fileExists(filepath.Join(dir, "setup.py")):
		return nil, fmt.Errorf(
			"в каталоге есть только setup.py — нужен pyproject.toml или setup.cfg: " +
				"setup.py это исполняемый код чужого автора, запускать его (и читать как текст) при импорте нельзя")
	default:
		return nil, fmt.Errorf(
			"в %s нет ни pyproject.toml, ни setup.cfg — нечего импортировать", dir)
	}

	p.PackageRoot = filepath.Clean(dir)
	p.Layout = detectLayout(dir, p)
	p.DescriptionFile = findDescriptionFile(dir, p.readmeRef)
	if p.DescriptionFile == "" && p.readmeRef != "" {
		p.notes = append(p.notes, fmt.Sprintf("readme объявлен как %q, а файла нет", p.readmeRef))
	}
	return p, nil
}

// ParsePyproject разбирает pyproject.toml. Ничего не знает о файловой системе:
// работает с байтами, которые уже прочитаны (тестам это удобно, а в Inspect
// файловая часть добавляется отдельно).
func ParsePyproject(data []byte) (*Project, error) {
	doc, err := parseToml(string(data))
	if err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	node, ok, err := doc.root.table("project")
	if err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf(
			"в pyproject.toml нет секции [project]: читается только PEP 621; " +
				"[tool.poetry] и другие бэкенды не разбираются — перенесите метаданные в [project]")
	}

	p := &Project{
		OptionalDependencies: map[string][]string{},
		Scripts:              map[string]string{},
		sourceFile:           "pyproject.toml",
	}
	if p.Name, _, err = node.stringField("name"); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if p.Name == "" {
		return nil, fmt.Errorf("в pyproject.toml [project] нет name — без имени плагин не назвать")
	}
	if p.Version, _, err = node.stringField("version"); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if p.Description, _, err = node.stringField("description"); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if p.RequiresPython, _, err = node.stringField("requires-python"); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if p.Dependencies, _, err = node.stringList("dependencies"); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if p.dynamic, _, err = node.stringList("dynamic"); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if err := readReadme(node, p); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if err := readOptionalDependencies(node, p); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	if err := readScripts(node, p); err != nil {
		return nil, fmt.Errorf("pyproject.toml: %w", err)
	}
	p.findWhere = findWhere(doc.root)
	// Раскладку по объявлению в файле: дальше Inspect уточнит её по диску
	// (detectLayout), но вызывающий ParsePyproject отдельно должен получить
	// осмысленный Layout, а не пустую строку.
	switch p.findWhere {
	case "src":
		p.Layout = LayoutSrcLayout
	case ".":
		p.Layout = LayoutFlat
	default:
		// Любой другой where мы не умеем трактовать и угадывать не будем:
		// неизвестная раскладка честнее догадки, а Inspect её потом уточнит.
		p.Layout = LayoutUnknown
		if p.findWhere != "" {
			p.notes = append(p.notes,
				fmt.Sprintf("packages.find where = %q — по такому объявлению раскладку не угадать", p.findWhere))
		}
	}
	if p.dynamicField("version") {
		p.notes = append(p.notes,
			"version объявлена в dynamic — значения в файле нет, а манифест WEDRA требует версию")
	}
	return p, nil
}

func readReadme(node *tomlNode, p *Project) error {
	child, ok := node.fields["readme"]
	if !ok || child.val == nil {
		return nil
	}
	switch child.val.kind {
	case tomlString:
		p.readmeRef = child.val.str
	case tomlInlineTable:
		for _, item := range child.val.items {
			if item.key == "file" && item.val.kind == tomlString {
				p.readmeRef = item.val.str
			}
		}
		if p.readmeRef == "" {
			p.notes = append(p.notes,
				"readme задан текстом прямо в pyproject.toml (не файлом) — описание плагина придётся писать руками")
		}
	default:
		return fmt.Errorf("строка %d: readme ждёт строку или { file = .. }, а в файле %s",
			child.line, kindName(child.val.kind))
	}
	return nil
}

func readOptionalDependencies(node *tomlNode, p *Project) error {
	opt, ok, err := node.table("optional-dependencies")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	for _, group := range opt.order {
		child := opt.fields[group]
		list, err := child.ownStringList(group)
		if err != nil {
			return err
		}
		p.OptionalDependencies[group] = list
	}
	return nil
}

// readScripts читает [project.scripts]. Значение обязано быть строкой
// "модуль:функция": если там таблица или число, молча выкинуть точку входа
// нельзя, а «придумать» её — тем более.
func readScripts(node *tomlNode, p *Project) error {
	scripts, ok, err := node.table("scripts")
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	for _, name := range scripts.order {
		child := scripts.fields[name]
		if child.val == nil {
			return fmt.Errorf("строка %d: точка входа %q должна быть строкой \"модуль:функция\", а не таблицей",
				child.line, name)
		}
		if child.val.kind != tomlString {
			return fmt.Errorf("строка %d: точка входа %q должна быть строкой \"модуль:функция\", а в файле %s",
				child.line, name, kindName(child.val.kind))
		}
		p.Scripts[name] = child.val.str
	}
	return nil
}

// findWhere достаёт подсказку о раскладке из
// [tool.setuptools.packages.find] where. Это объявление самого файла, а не
// догадка: раскладку потом уточняет Inspect по диску (detectLayout).
func findWhere(root *tomlNode) string {
	tool, ok, err := root.table("tool")
	if err != nil || !ok {
		return ""
	}
	setuptools, ok, err := tool.table("setuptools")
	if err != nil || !ok {
		return ""
	}
	packages, ok, err := setuptools.table("packages")
	if err != nil || !ok {
		return ""
	}
	find, ok, err := packages.table("find")
	if err != nil || !ok {
		return ""
	}
	list, ok, err := find.stringList("where")
	if err != nil || !ok || len(list) == 0 {
		return ""
	}
	return strings.TrimSpace(filepath.ToSlash(list[0]))
}

// detectLayout определяет раскладку исходников: лежат ли пакеты в корне (flat)
// или в src/ (src-layout). Порядок доверия: сначала объявленный в файле where,
// потом содержимое каталога, потому что объявление может врать (а мы это
// переживём тихо только в том случае, если на диске согласуется).
func detectLayout(dir string, p *Project) string {
	srcDir := filepath.Join(dir, "src")
	srcExists := isDir(srcDir)
	rootModule := moduleNameFor(p.Name)

	switch {
	case p.findWhere == "src":
		if srcExists {
			return LayoutSrcLayout
		}
		p.notes = append(p.notes,
			"в pyproject/setup.cfg объявлено where = src, а каталога src нет — раскладку считаю неизвестной")
		return LayoutUnknown
	case srcExists && (rootModule == "" || isDir(filepath.Join(srcDir, rootModule)) || hasPythonPackage(srcDir)):
		return LayoutSrcLayout
	case rootModule != "" && fileExists(filepath.Join(dir, rootModule, "__init__.py")):
		return LayoutFlat
	case hasPythonPackage(dir):
		return LayoutFlat
	default:
		p.notes = append(p.notes,
			"не нашёл ни src/, ни пакета в корне — раскладка неизвестна, sys.path будет пустым")
		return LayoutUnknown
	}
}

// hasPythonPackage ищет __init__.py в непосредственных подкаталогах, игнорируя
// служебные каталоги. Топ-одного уровня достаточно: в src-layout второй
// уровень редко, а глубокий обход случайно находит тесты и примеры.
func hasPythonPackage(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() || skipDir(entry.Name()) {
			continue
		}
		if fileExists(filepath.Join(dir, entry.Name(), "__init__.py")) {
			return true
		}
	}
	return false
}

func skipDir(name string) bool {
	switch {
	case name == "src" || name == "tests" || name == "test" || name == "docs" || name == "doc":
		return true
	case name == "examples" || name == "example" || name == "scripts" || name == "tools":
		return true
	case name == "build" || name == "dist" || name == "venv" || name == ".venv":
		return true
	case strings.HasPrefix(name, "."):
		return true
	default:
		return false
	}
}

// findDescriptionFile ищет README: сначала объявленный в метаданных, потом
// обычный порядок. Содержимое не читается — оно может быть большим, а нам
// нужен факт наличия и путь.
func findDescriptionFile(dir, declared string) string {
	if declared != "" {
		candidate := filepath.Join(dir, filepath.FromSlash(declared))
		if fileExists(candidate) {
			return candidate
		}
	}
	for _, name := range []string{"README.md", "README.rst", "README.txt"} {
		candidate := filepath.Join(dir, name)
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

// moduleNameFor переводит имя дистрибутива в имя пакета, как это делает pip:
// «telegram-outreach» → «telegram_outreach». Точное совпадение не
// гарантировано (бывает «zope.interface» → «zope.interface», а «Django» →
// «django»), поэтому результат используется только как подсказка поиска
// каталога, а не как факт.
func moduleNameFor(dist string) string {
	return strings.ReplaceAll(normalizeDistName(dist), "-", "_")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// sortedKeys — детерминированный порядок обхода map'ов. Без этого план для
// одного и того же проекта печатал бы группы и точки входа в случайном
// порядке, и diff двух запусков показывал бы изменения, которых не было.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
