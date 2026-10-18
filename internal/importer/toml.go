package importer

import (
	"fmt"
	"regexp"
	"strings"
)

// Минимальный разбор TOML — ровно того подмножества, которое встречается в
// pyproject.toml и setup.cfg чужого проекта. Отдельная библиотека в go.mod не
// добавлена сознательно: в репозитории жёсткая дисциплина пинов (см. go.mod),
// и одна зависимость ради файла, который никто не будет редактировать руками,
// того не стоит. Цена этого решения — подмножество, и она должна быть видна.
//
// Правило, которому разбор подчинён: ЛЮБАЯ конструкция, которую мы не поняли,
// поднимает ошибку с номером строки. Тихий пропуск здесь недопустим: импортёр
// читает чужой проект и строит по нему манифест, поэтому «не разобрал и не
// сказал» превращается в плагин, который молча работает не с теми
// зависимостями. Лучше отказ с текстом «строка 41: конструкция такая-то не
// поддерживается», чем дыра в зависимостях, о которой узнают на проде.
//
// Чего разбор НЕ умеет и говорит об этом ошибкой:
//   - массивы таблиц `[[tool.poetry.packages]]`;
//   - даты и время (`1979-05-27T07:32:00Z`) — для метаданных они не нужны,
//     а молча превращать их в строку значило бы выдумывать значение;
//   - ключи с не-ASCII буквами вне кавычек — на них сразу отказ, потому что
//     молча пропустить такой ключ означало бы не заметить опечатку автора.
//
// Умеет: заголовки таблиц (в том числе точечные), точечные ключи
// (`tool.ruff.line-length = 100`), `key = value` со строкой (обычной,
// буквальной, многострочной), числом, true/false, массивом (в том числе
// многострочным), встроенной таблицей `{ ... }` и комментарии `#`.

type tomlKind int

const (
	tomlString tomlKind = iota
	tomlNumber
	tomlBool
	tomlArray
	tomlInlineTable
)

type tomlValue struct {
	kind  tomlKind
	str   string      // строка, булево и число — текстом как в файле
	list  []tomlValue // массив
	items []tomlItem  // встроенная таблица: порядок исходного текста
}

type tomlItem struct {
	key string
	val tomlValue
}

// tomlNode — узел документа. val != nil означает «это значение», иначе узел
// таблица и держит поля. Разделение нужно, чтобы отличать `a = 1` от `[a]`:
// переопределение в TOML запрещено, и мы это отвергаем loudly, а не затираем
// первое значение вторым.
type tomlNode struct {
	line     int
	declared bool // объявлена именно заголовком [..]: повтор такого заголовка — дубликат
	val      *tomlValue
	fields   map[string]*tomlNode
	order    []string
}

type tomlDoc struct {
	root *tomlNode
}

func newTomlNode(line int) *tomlNode {
	return &tomlNode{line: line, fields: map[string]*tomlNode{}}
}

// table возвращает дочернюю таблицу. Если узел есть, но это значение —
// ошибка: читатель рассчитывал на таблицу и получил «не то», а молча
// продолжить означало бы потерять содержимое.
func (n *tomlNode) table(name string) (*tomlNode, bool, error) {
	c, ok := n.fields[name]
	if !ok {
		return nil, false, nil
	}
	if c.val != nil {
		return nil, false, fmt.Errorf("строка %d: %q задано значением, а читатель ждёт таблицу", c.line, name)
	}
	return c, true, nil
}

// stringField читает строковое поле. Число принимается текстом: `version = 1.0`
// в pyproject встречается (PEP 621 требует строку, но setuptools такой
// записи не отвергает) и выбрасывать такое значение молча нельзя.
func (n *tomlNode) stringField(name string) (string, bool, error) {
	c, ok := n.fields[name]
	if !ok || c.val == nil {
		return "", false, nil
	}
	switch c.val.kind {
	case tomlString:
		return c.val.str, true, nil
	case tomlNumber:
		return c.val.str, true, nil
	default:
		return "", false, fmt.Errorf("строка %d: %q ждёт строку, а в файле %s", c.line, name, kindName(c.val.kind))
	}
}

// stringList читает массив строк. Элемент не-строка — ошибка: зависимость,
// которая не строка, в `runtime.requires` не попадёт, а молча её пропустить
// значит потерять пакет.
func (n *tomlNode) stringList(name string) ([]string, bool, error) {
	c, ok := n.fields[name]
	if !ok || c.val == nil {
		return nil, false, nil
	}
	if c.val.kind != tomlArray {
		return nil, false, fmt.Errorf("строка %d: %q ждёт массив строк, а в файле %s", c.line, name, kindName(c.val.kind))
	}
	out := make([]string, 0, len(c.val.list))
	for i, item := range c.val.list {
		if item.kind != tomlString && item.kind != tomlNumber {
			return nil, false, fmt.Errorf("строка %d: %q[%d] ждёт строку, а в файле %s", c.line, name, i, kindName(item.kind))
		}
		out = append(out, item.str)
	}
	return out, true, nil
}

// ownStringList читает значение самого узла как массив строк. Нужно там, где
// значение лежит на самой группе, а не на её подполе: в
// [project.optional-dependencies] группа `dev` — это сам массив.
func (n *tomlNode) ownStringList(label string) ([]string, error) {
	if n.val == nil {
		return nil, fmt.Errorf("строка %d: %q ждёт массив строк, а это таблица", n.line, label)
	}
	if n.val.kind != tomlArray {
		return nil, fmt.Errorf("строка %d: %q ждёт массив строк, а в файле %s", n.line, label, kindName(n.val.kind))
	}
	out := make([]string, 0, len(n.val.list))
	for i, item := range n.val.list {
		if item.kind != tomlString && item.kind != tomlNumber {
			return nil, fmt.Errorf("строка %d: %q[%d] ждёт строку, а в файле %s", n.line, label, i, kindName(item.kind))
		}
		out = append(out, item.str)
	}
	return out, nil
}

func kindName(k tomlKind) string {
	switch k {
	case tomlString:
		return "строка"
	case tomlNumber:
		return "число"
	case tomlBool:
		return "true/false"
	case tomlArray:
		return "массив"
	case tomlInlineTable:
		return "таблица { .. }"
	default:
		return "неизвестное значение"
	}
}

// setPath записывает значение по точечному пути. Дубликаты и попытки накрыть
// значение таблицей (или наоборот) — ошибка: TOML такое запрещает, и «последний
// выигрывает» здесь ознал бы, что метаданные читаются не те.
func (n *tomlNode) setPath(path []string, val tomlValue, line int) error {
	cur := n
	for i, seg := range path {
		last := i == len(path)-1
		next, ok := cur.fields[seg]
		if !ok {
			next = newTomlNode(line)
			cur.fields[seg] = next
			cur.order = append(cur.order, seg)
		}
		if last {
			switch {
			case next.val != nil:
				return fmt.Errorf("строка %d: дубликат ключа %q (уже задан на строке %d)", line, seg, next.line)
			case next.declared:
				return fmt.Errorf("строка %d: %q уже объявлено таблицей на строке %d — значением перекрыть нельзя",
					line, seg, next.line)
			}
			v := val
			next.val = &v
			return nil
		}
		if next.val != nil {
			return fmt.Errorf("строка %d: %q уже задано значением на строке %d, продолжить путь нельзя",
				line, seg, next.line)
		}
		cur = next
	}
	return nil
}

// attachTable создаёт или находит таблицу по точечному пути из заголовка.
// attachTable создаёт или находит таблицу по точечному пути из заголовка.
// Повторный заголовок — ошибка, а вот заголовок после точечного ключа
// (`tool.a = 1`, потом `[tool]`) — нет: точечный ключ таблицу только создаёт,
// и в TOML это переоткрытие, а не дубликат.
func (d *tomlDoc) attachTable(path []string, line int) (*tomlNode, error) {
	cur := d.root
	for i, seg := range path {
		next, ok := cur.fields[seg]
		if !ok {
			next = newTomlNode(line)
			cur.fields[seg] = next
			cur.order = append(cur.order, seg)
		}
		if next.val != nil {
			return nil, fmt.Errorf("строка %d: %q уже задано значением на строке %d — таблицей объявить нельзя",
				line, seg, next.line)
		}
		if i == len(path)-1 {
			if next.declared {
				return nil, fmt.Errorf("строка %d: таблица %q уже объявлена на строке %d", line, seg, next.line)
			}
			next.declared = true
		}
		cur = next
	}
	return cur, nil
}

func parseToml(src string) (*tomlDoc, error) {
	sc := newScanner(src)
	doc := &tomlDoc{root: newTomlNode(0)}
	cur := doc.root
	for {
		sc.skipToNextStatement()
		if sc.eof() {
			return doc, nil
		}
		if sc.peek() == '[' {
			path, line, err := sc.readTableHeader()
			if err != nil {
				return nil, err
			}
			node, err := doc.attachTable(path, line)
			if err != nil {
				return nil, err
			}
			cur = node
			continue
		}
		path, val, err := sc.readKeyValue()
		if err != nil {
			return nil, err
		}
		if err := cur.setPath(path, val, sc.line); err != nil {
			return nil, err
		}
	}
}

// scanner — курсор по исходному тексту с номером строки. Номера строк
// обязательны: сообщение «не понял конструкцию» без места бесполезно, а
// pyproject чужого проекта читают вслепую.
type scanner struct {
	src  string
	pos  int
	line int
}

func newScanner(src string) *scanner {
	// спецсимволы BOM в начале молча ломали бы первый заголовок
	src = strings.TrimPrefix(src, "\ufeff")
	return &scanner{src: src, line: 1}
}

func (s *scanner) eof() bool { return s.pos >= len(s.src) }

func (s *scanner) peek() byte {
	if s.eof() {
		return 0
	}
	return s.src[s.pos]
}

func (s *scanner) peekAt(off int) byte {
	if s.pos+off >= len(s.src) {
		return 0
	}
	return s.src[s.pos+off]
}

func (s *scanner) advance() byte {
	c := s.src[s.pos]
	s.pos++
	if c == '\n' {
		s.line++
	}
	return c
}

func (s *scanner) skipSpaces() {
	for !s.eof() && (s.peek() == ' ' || s.peek() == '\t') {
		s.advance()
	}
}

func (s *scanner) skipComment() {
	for !s.eof() && s.peek() != '\n' {
		s.advance()
	}
}

// skipToNextStatement переходит к следующему заголовку или паре ключ-значение:
// только пробелы, пустые строки и комментарии. Ничего другого между
// конструкциями TOML не допускает, поэтому встреченный символ — это начало
// следующей конструкции.
func (s *scanner) skipToNextStatement() {
	for !s.eof() {
		switch c := s.peek(); {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.advance()
		case c == '#':
			s.skipComment()
		default:
			return
		}
	}
}

// endOfStatement проверяет, что после значения на строке ничего нет. Именно
// здесь ловится битый TOML вида `version = 0.1.0`: вместо тихой потери версии
// получается ошибка с номером строки.
func (s *scanner) endOfStatement() error {
	s.skipSpaces()
	if s.eof() {
		return nil
	}
	switch c := s.peek(); c {
	case '#':
		s.skipComment()
		return nil
	case '\r':
		s.advance()
		if s.eof() {
			return nil
		}
		if s.peek() != '\n' {
			return fmt.Errorf("строка %d: после значения ожидался конец строки, а здесь %q", s.line, string(s.peek()))
		}
		s.advance()
		return nil
	case '\n':
		s.advance()
		return nil
	default:
		return fmt.Errorf("строка %d: после значения ожидался конец строки или комментарий, а здесь %q",
			s.line, string(c))
	}
}

func isBareKeyChar(c byte) bool {
	return c == '-' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isValueTerminator(c byte) bool {
	return c == 0 || c == ' ' || c == '\t' || c == '\n' || c == '\r' ||
		c == ',' || c == ']' || c == '}' || c == '#'
}

// readTableHeader читает `[a.b.c]`. Возвращает путь сегментов и номер строки
// заголовка — по нему потом сообщается о дубликатах и конфликтах.
func (s *scanner) readTableHeader() ([]string, int, error) {
	line := s.line
	s.advance() // '['
	if s.peek() == '[' {
		return nil, 0, fmt.Errorf(
			"строка %d: массив таблиц [[..]] не поддерживается — конструкция не разобрана, а пропустить её молча нельзя",
			line)
	}
	path, err := s.readKeyPath()
	if err != nil {
		return nil, 0, err
	}
	if s.peek() != ']' {
		return nil, 0, fmt.Errorf("строка %d: в заголовке таблицы жду ']', а здесь %q (кавычки в заголовке не поддержаны)",
			s.line, string(s.peek()))
	}
	s.advance()
	if err := s.endOfStatement(); err != nil {
		return nil, 0, err
	}
	return path, line, nil
}

// readKeyPath читает ключ или точечный путь ключей: `name`, `a.b`,
// `"quoted.key"`. Голый точечный ключ вне заголовка разрешён TOML, но не нами:
// см. doc-комментарий к parseToml, конструкция поднимает ошибку.
func (s *scanner) readKeyPath() ([]string, error) {
	var path []string
	for {
		seg, err := s.readKeySegment()
		if err != nil {
			return nil, err
		}
		path = append(path, seg)
		if s.peek() == '.' {
			s.advance()
			continue
		}
		return path, nil
	}
}

func (s *scanner) readKeySegment() (string, error) {
	switch s.peek() {
	case '"', '\'':
		return s.readString(false)
	default:
		start := s.pos
		for !s.eof() && isBareKeyChar(s.peek()) {
			s.advance()
		}
		if start == s.pos {
			return "", fmt.Errorf("строка %d: ожидался ключ, а здесь %q", s.line, string(s.peek()))
		}
		return s.src[start:s.pos], nil
	}
}

func (s *scanner) readKeyValue() ([]string, tomlValue, error) {
	line := s.line
	path, err := s.readKeyPath()
	if err != nil {
		return nil, tomlValue{}, err
	}
	s.skipSpaces()
	if s.peek() != '=' {
		return nil, tomlValue{}, fmt.Errorf("строка %d: после ключа %q жду '=', а здесь %q",
			line, strings.Join(path, "."), string(s.peek()))
	}
	s.advance()
	s.skipSpaces()
	val, err := s.readValue()
	if err != nil {
		return nil, tomlValue{}, err
	}
	if err := s.endOfStatement(); err != nil {
		return nil, tomlValue{}, err
	}
	return path, val, nil
}

func (s *scanner) readValue() (tomlValue, error) {
	if s.eof() {
		return tomlValue{}, fmt.Errorf("строка %d: после '=' ожидалось значение, а файл кончился", s.line)
	}
	switch c := s.peek(); {
	case c == '"':
		multiline := s.peekAt(1) == '"' && s.peekAt(2) == '"'
		str, err := s.readString(multiline)
		if err != nil {
			return tomlValue{}, err
		}
		return tomlValue{kind: tomlString, str: str}, nil
	case c == '\'':
		multiline := s.peekAt(1) == '\'' && s.peekAt(2) == '\''
		str, err := s.readString(multiline)
		if err != nil {
			return tomlValue{}, err
		}
		return tomlValue{kind: tomlString, str: str}, nil
	case c == '[':
		return s.readArray()
	case c == '{':
		return s.readInlineTable()
	case c == 't' || c == 'f':
		return s.readBool()
	case c == '+' || c == '-' || (c >= '0' && c <= '9'):
		return s.readNumber()
	default:
		return tomlValue{}, fmt.Errorf(
			"строка %d: значение начинается с %q — ждал строку, число, true/false, массив или таблицу { .. }",
			s.line, string(c))
	}
}

func (s *scanner) readBool() (tomlValue, error) {
	for _, word := range []string{"true", "false"} {
		if strings.HasPrefix(s.src[s.pos:], word) {
			after := s.peekAt(len(word))
			if !isValueTerminator(after) && after != '=' {
				continue
			}
			for range word {
				s.advance()
			}
			return tomlValue{kind: tomlBool, str: word}, nil
		}
	}
	return tomlValue{}, fmt.Errorf("строка %d: не понимаю значение, начинающееся с %q", s.line, string(s.peek()))
}

// numberRe — форматы TOML-числа. Текст сохраняется как есть: превращать
// `2.0.30` (в строковом виде) в число и обратно — значит рисковать потерей
// символов версии.
var numberRe = regexp.MustCompile(`^[+-]?(?:0x[0-9A-Fa-f](?:_?[0-9A-Fa-f])*|0o[0-7](?:_?[0-7])*|` +
	`0b[01](?:_?[01])*|[0-9](?:_?[0-9])*(?:\.[0-9](?:_?[0-9])*)?(?:[eE][+-]?[0-9](?:_?[0-9])*)?)$`)

func isNumberChar(c byte) bool {
	// x/o/b нужны для префиксов 0x/0o/0b, а e/E — для экспоненты. Остальные
	// буквы в число не пускаем: `1_000abc` должен стать ошибкой, а не
	// молчаливым «числом с мусором».
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') ||
		c == 'x' || c == 'X' || c == 'o' || c == 'O' || c == 'b' || c == 'B' ||
		c == 'e' || c == 'E' ||
		c == '_' || c == '.' || c == '+' || c == '-'
}

func (s *scanner) readNumber() (tomlValue, error) {
	start := s.pos
	for !s.eof() && isNumberChar(s.peek()) {
		s.advance()
	}
	raw := s.src[start:s.pos]
	if !numberRe.MatchString(raw) {
		return tomlValue{}, fmt.Errorf("строка %d: %q не число и не строка — если это версия, её надо взять в кавычки",
			s.line, raw)
	}
	return tomlValue{kind: tomlNumber, str: raw}, nil
}

// readArray читает `[ ... ]`, в том числе многострочный. Переводы строк и
// комментарии внутри допустимы — так пишут длинные списки зависимостей.
func (s *scanner) readArray() (tomlValue, error) {
	line := s.line
	s.advance() // '['
	out := tomlValue{kind: tomlArray}
	for {
		s.skipArrayFiller()
		if s.eof() {
			return tomlValue{}, fmt.Errorf("строка %d: массив не закрыт", line)
		}
		if s.peek() == ']' {
			s.advance()
			return out, nil
		}
		val, err := s.readValue()
		if err != nil {
			return tomlValue{}, err
		}
		out.list = append(out.list, val)
		s.skipArrayFiller()
		if s.eof() {
			return tomlValue{}, fmt.Errorf("строка %d: массив не закрыт", line)
		}
		switch s.peek() {
		case ',':
			s.advance()
		case ']':
			s.advance()
			return out, nil
		default:
			return tomlValue{}, fmt.Errorf("строка %d: в массиве ждал ',' или ']', а здесь %q",
				s.line, string(s.peek()))
		}
	}
}

func (s *scanner) skipArrayFiller() {
	for !s.eof() {
		switch c := s.peek(); {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.advance()
		case c == '#':
			s.skipComment()
		default:
			return
		}
	}
}

// readInlineTable читает `{ key = value, .. }`. Встроенная таблица нужна для
// `authors = [{ name = "..." }]` и `license = { text = "MIT" }`, без них
// половина реальных pyproject не проходит. Перевод строки внутри неё запрещён
// TOML — поднимаем ошибку, а не дочитываем как угодно.
func (s *scanner) readInlineTable() (tomlValue, error) {
	line := s.line
	s.advance() // '{'
	out := tomlValue{kind: tomlInlineTable}
	for {
		s.skipSpaces()
		if s.eof() {
			return tomlValue{}, fmt.Errorf("строка %d: встроенная таблица не закрыта", line)
		}
		if s.peek() == '}' {
			s.advance()
			return out, nil
		}
		if s.peek() == '\n' || s.peek() == '\r' {
			return tomlValue{}, fmt.Errorf(
				"строка %d: перевод строки внутри { .. } запрещён TOML — вероятно, таблица не дописана", s.line)
		}
		key, err := s.readKeySegment()
		if err != nil {
			return tomlValue{}, err
		}
		s.skipSpaces()
		if s.peek() != '=' {
			return tomlValue{}, fmt.Errorf("строка %d: во встроенной таблице после ключа %q жду '=', а здесь %q",
				s.line, key, string(s.peek()))
		}
		s.advance()
		s.skipSpaces()
		val, err := s.readValue()
		if err != nil {
			return tomlValue{}, err
		}
		out.items = append(out.items, tomlItem{key: key, val: val})
		s.skipSpaces()
		if s.eof() {
			return tomlValue{}, fmt.Errorf("строка %d: встроенная таблица не закрыта", line)
		}
		switch s.peek() {
		case ',':
			s.advance()
		case '}':
			s.advance()
			return out, nil
		default:
			return tomlValue{}, fmt.Errorf("строка %d: во встроенной таблице ждал ',' или '}', а здесь %q",
				s.line, string(s.peek()))
		}
	}
}

// readString читает строку. quote задаёт тип: '"' — базовая с escape-последовательностями,
// '\” — буквальная, содержимое как есть.
func (s *scanner) readString(multiline bool) (string, error) {
	line := s.line
	quote := s.peek()
	basic := quote == '"'
	if multiline {
		s.advance()
		s.advance()
		s.advance()
		// перевод строки сразу после открывающей тройки отбрасывается
		if s.peek() == '\r' {
			s.advance()
		}
		if s.peek() == '\n' {
			s.advance()
		}
	} else {
		s.advance()
	}
	var b strings.Builder
	for {
		if s.eof() {
			return "", fmt.Errorf("строка %d: строка не закрыта", line)
		}
		c := s.peek()
		switch {
		case c == quote && multiline:
			// серия кавычек: три и больше закрывают строку, лишние — содержимое
			run := 0
			for s.peekAt(run) == quote {
				run++
			}
			for range run {
				s.advance()
			}
			if run >= 3 {
				// лишние кавычки серии (4 и больше) относятся к содержимому
				b.WriteString(strings.Repeat(string(quote), run-3))
				return b.String(), nil
			}
			b.WriteString(strings.Repeat(string(quote), run))
		case c == quote:
			s.advance()
			return b.String(), nil
		case c == '\\' && basic:
			if err := s.readEscape(&b, multiline); err != nil {
				return "", err
			}
		case c == '\n' && !multiline:
			return "", fmt.Errorf("строка %d: необработанный перевод строки внутри строки (нужны \"\"\"..\"\"\")",
				s.line)
		case c == '\n' || c == '\r':
			// в многострочной строке перевод строки — обычное содержимое
			b.WriteByte(s.advance())
		case c < 0x20 && c != '\t':
			return "", fmt.Errorf("строка %d: недопустимый управляющий символ 0x%02x в строке", s.line, c)
		default:
			b.WriteByte(s.advance())
		}
	}
}

func (s *scanner) readEscape(b *strings.Builder, multiline bool) error {
	line := s.line
	// Сначала съедаем сам обратный слэш, потом читаем символ после него.
	// Если этого не сделать, обработчик увидит '\\' и запишет его обратно, а
	// escape-последовательность останется обычным текстом.
	s.advance()
	if s.eof() {
		return fmt.Errorf("строка %d: строка кончилась сразу после обратного слэша", line)
	}
	c := s.advance()
	switch c {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case '"':
		b.WriteByte('"')
	case '\'':
		b.WriteByte('\'')
	case '\\':
		b.WriteByte('\\')
	case 'u':
		return s.readUnicodeEscape(b, 'u', 4, line)
	case 'U':
		return s.readUnicodeEscape(b, 'U', 8, line)
	case '\n', '\r':
		// «продолжение строки»: backslash в конце строки срезает все следующие
		// пробельные символы вместе с переводами строк
		if !multiline {
			return fmt.Errorf("строка %d: перенос строки через \\ разрешён только в \"\"\"..\"\"\"", line)
		}
		for !s.eof() {
			c := s.peek()
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				s.advance()
				continue
			}
			return nil
		}
		return nil
	default:
		return fmt.Errorf("строка %d: неизвестная escape-последовательность \"\\%c\"", line, c)
	}
	return nil
}

func (s *scanner) readUnicodeEscape(b *strings.Builder, letter byte, digits int, line int) error {
	var r rune
	for range digits {
		if s.eof() {
			return fmt.Errorf("строка %d: escape \\%c не дописан (жду %d hex-символов)", line, letter, digits)
		}
		c := s.advance()
		digit, ok := hexDigit(c)
		if !ok {
			return fmt.Errorf("строка %d: в escape \\%c ожидалось %d hex-символов, а здесь %q",
				line, letter, digits, string(c))
		}
		r = r*16 + rune(digit)
	}
	if r > 0x10FFFF || (r >= 0xD800 && r <= 0xDFFF) {
		return fmt.Errorf("строка %d: escape даёт недопустимый код %U", line, r)
	}
	b.WriteRune(r)
	return nil
}

func hexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}
