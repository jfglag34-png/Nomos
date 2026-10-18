package importer

import (
	"fmt"
	"strings"
)

// Запасной путь для проектов без pyproject.toml: setup.cfg. Это конфигурация,
// а не код, её безопасно читать. Состав поддержан тот же, что и у PEP 621:
// [metadata] (name/version/description/long_description), [options]
// (python_requires/install_requires/extras_require/packages_dir),
// [options.packages.find] where, [options.entry_points] console_scripts.
//
// Разбор — минимальный INI, потому что чужой setup.cfg это тоже чужой файл и
// относиться к нему надо так же: незнакомая конструкция — отказ с номером
// строки. Молча перепрыгнуть через строку, которую мы не поняли, здесь
// опасно вдвойне: в setup.cfg именно так выглядит `install_requires` с
// продолжениями, и потеря одной строки означает потерянную зависимость.

// iniValue — значение ключа. first — первая строка, rest — продолжения
// (строки с отступом). Установщикам нужны именно продолжения: длинный
// install_requires пишут по одной зависимости на строку.
type iniValue struct {
	line  int
	first string
	rest  []string
}

func (v *iniValue) lines() []string {
	out := make([]string, 0, len(v.rest)+1)
	if strings.TrimSpace(v.first) != "" {
		out = append(out, strings.TrimSpace(v.first))
	}
	for _, line := range v.rest {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

type iniDoc struct {
	sections map[string]*iniSection
	order    []string
}

type iniSection struct {
	line   int
	values map[string]*iniValue
	keys   []string
}

func (d *iniDoc) section(name string) (*iniSection, bool) {
	s, ok := d.sections[name]
	return s, ok
}

func (s *iniSection) value(name string) (*iniValue, bool) {
	v, ok := s.values[name]
	return v, ok
}

// parseIni разбирает setup.cfg. Секции, которые нам не нужны ([egg_info],
// [bdist_wheel], ...), пропускаются молча: они разобраны и поняты, просто не
// относятся к метаданным. Незнаная синтаксическая конструкция — ошибка.
func parseIni(src string) (*iniDoc, error) {
	doc := &iniDoc{sections: map[string]*iniSection{}}
	var current *iniSection
	var lastKey string

	lines := strings.Split(src, "\n")
	for i, raw := range lines {
		lineNo := i + 1
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			if !strings.HasSuffix(trimmed, "]") {
				return nil, fmt.Errorf("строка %d: заголовок секции не закрыт: %q", lineNo, trimmed)
			}
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			if name == "" {
				return nil, fmt.Errorf("строка %d: пустое имя секции", lineNo)
			}
			if _, dup := doc.sections[name]; dup {
				return nil, fmt.Errorf("строка %d: секция [%s] объявлена повторно", lineNo, name)
			}
			current = &iniSection{line: lineNo, values: map[string]*iniValue{}}
			doc.sections[name] = current
			doc.order = append(doc.order, name)
			lastKey = ""
			continue
		}
		if current == nil {
			return nil, fmt.Errorf("строка %d: значение %q стоит до какой-либо секции", lineNo, trimmed)
		}
		indented := line != "" && (line[0] == ' ' || line[0] == '\t')
		sep := strings.IndexAny(trimmed, "=:")
		if !indented && sep > 0 {
			key := strings.TrimSpace(trimmed[:sep])
			value := strings.TrimSpace(trimmed[sep+1:])
			if key == "" {
				return nil, fmt.Errorf("строка %d: пустое имя параметра", lineNo)
			}
			if _, dup := current.values[key]; dup {
				return nil, fmt.Errorf("строка %d: параметр %q в [%s] задан повторно",
					lineNo, key, sectionName(doc, current))
			}
			current.values[key] = &iniValue{line: lineNo, first: value}
			current.keys = append(current.keys, key)
			lastKey = key
			continue
		}
		// Продолжение значения. Оно обязано быть с отступом: иначе строка без
		// «=» тихо приклеилась бы к предыдущему параметру (так ведёт себя
		// configparser, но там же — источник тихих потерянных зависимостей),
		// а пропускать такую строку молча нельзя.
		if !indented || lastKey == "" {
			return nil, fmt.Errorf("строка %d: не понимаю строку %q (ожидался ключ или продолжение с отступом)",
				lineNo, trimmed)
		}
		current.values[lastKey].rest = append(current.values[lastKey].rest, line)
	}
	return doc, nil
}

func sectionName(doc *iniDoc, target *iniSection) string {
	for _, name := range doc.order {
		if doc.sections[name] == target {
			return name
		}
	}
	return "?"
}

// parseSetupCfg читает метаданные из setup.cfg в ту же структуру, что и из
// pyproject.toml, чтобы дальше по коду разницы не было.
func parseSetupCfg(data []byte) (*Project, error) {
	doc, err := parseIni(string(data))
	if err != nil {
		return nil, err
	}
	meta, ok := doc.section("metadata")
	if !ok {
		return nil, fmt.Errorf("в setup.cfg нет секции [metadata] — нечего импортировать")
	}
	p := &Project{
		OptionalDependencies: map[string][]string{},
		Scripts:              map[string]string{},
		sourceFile:           "setup.cfg",
	}
	if v, ok := meta.value("name"); ok {
		p.Name = strings.TrimSpace(v.first)
	}
	if p.Name == "" {
		return nil, fmt.Errorf("в [metadata] нет name — без имени плагин не назвать")
	}
	if v, ok := meta.value("version"); ok {
		p.Version = strings.TrimSpace(v.first)
	}
	if v, ok := meta.value("description"); ok {
		p.Description = strings.TrimSpace(v.first)
	}
	if v, ok := meta.value("long_description"); ok {
		// `long_description = file: README.md` — ссылка на файл; вариант
		// «самим текстом» описание проекта не даёт и в план не идёт.
		if value := strings.TrimSpace(v.first); strings.HasPrefix(value, "file:") {
			p.readmeRef = strings.TrimSpace(strings.TrimPrefix(value, "file:"))
		} else if value != "" {
			p.notes = append(p.notes,
				"long_description задан текстом прямо в setup.cfg — описание плагина придётся писать руками")
		}
	}

	options, hasOptions := doc.section("options")
	if hasOptions {
		if v, ok := options.value("python_requires"); ok {
			p.RequiresPython = strings.TrimSpace(v.first)
		}
		if v, ok := options.value("install_requires"); ok {
			p.Dependencies = v.lines()
		}
		if v, ok := options.value("packages"); ok && strings.HasPrefix(strings.TrimSpace(v.first), "find:") {
			if find, ok := doc.section("options.packages.find"); ok {
				if w, ok := find.value("where"); ok {
					p.findWhere = strings.TrimSpace(w.first)
				}
			}
			if p.findWhere == "" {
				p.findWhere = "."
			}
		}
	}
	if err := readSetupExtras(doc, p); err != nil {
		return nil, err
	}
	if err := readSetupEntryPoints(doc, p); err != nil {
		return nil, err
	}
	return p, nil
}

// readSetupExtras читает [options.extras_require]: группы необязательных
// зависимостей. Формат строки — «группа = a, b, c», и это заметная деталь
// синтаксиса: запятая значит РАЗДЕЛЕНИЕ ЗАВИСИМОСТЕЙ, а запятая внутри
// ограничения версии (`a>=1.0,<2.0`) экранируется кавычками.
func readSetupExtras(doc *iniDoc, p *Project) error {
	extras, ok := doc.section("options.extras_require")
	if !ok {
		return nil
	}
	for _, group := range extras.keys {
		v := extras.values[group]
		p.OptionalDependencies[group] = splitExtraItems(v)
	}
	return nil
}

func splitExtraItems(v *iniValue) []string {
	var out []string
	for _, line := range v.lines() {
		out = append(out, splitExtrasLine(line)...)
	}
	return out
}

// splitExtrasLine разбирает «группа = a, b, "c<2,d", e». В setup.cfg
// зависимость с запятой внутри обязана быть в кавычках, иначе это две
// зависимости — так и читаем, но молча склеивать их обратно нельзя.
func splitExtrasLine(line string) []string {
	var out []string
	var current strings.Builder
	inQuotes := false
	flush := func() {
		if item := strings.TrimSpace(current.String()); item != "" {
			out = append(out, item)
		}
		current.Reset()
	}
	for i := range len(line) {
		c := line[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case c == ',' && !inQuotes:
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return out
}

// readSetupEntryPoints читает [options.entry_points] console_scripts. Без них
// проект на setup.cfg всегда давал бы блокер «нет точки входа», хотя точка
// входа в файле написана.
func readSetupEntryPoints(doc *iniDoc, p *Project) error {
	entries, ok := doc.section("options.entry_points")
	if !ok {
		return nil
	}
	for _, group := range entries.keys {
		v := entries.values[group]
		if group != "console_scripts" {
			p.notes = append(p.notes,
				fmt.Sprintf("группа точек входа %q в setup.cfg проигнорирована — берём только console_scripts", group))
			continue
		}
		for _, item := range v.lines() {
			sep := strings.Index(item, "=")
			if sep <= 0 {
				return fmt.Errorf("строка %d: в console_scripts не понимаю %q (ожидалось \"имя = модуль:функция\")",
					v.line, item)
			}
			name := strings.TrimSpace(item[:sep])
			target := strings.TrimSpace(item[sep+1:])
			if name == "" || target == "" {
				return fmt.Errorf("строка %d: в console_scripts не понимаю %q (ожидалось \"имя = модуль:функция\")",
					v.line, item)
			}
			p.Scripts[name] = target
		}
	}
	return nil
}
