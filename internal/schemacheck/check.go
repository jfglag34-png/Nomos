// Package schemacheck проверяет, что опубликованные JSON Schema соответствуют
// тому, что лежит в репозитории.
//
// Зачем: schemas/ четыре года(?) никем не читались — ни одной строки Go на
// каталог не ссылалось. При этом схема разошлась с кодом в обе стороны:
// `loop` был выпущен и исполнялся движком, но отсутствовал в схеме, а
// additionalProperties:false делал такой пайплайн невалидным по контракту.
//
// ВАЛИДАТОР — ПОДМНОЖЕСТВО draft-07, без зависимостей. Правило, которому он
// подчинён: незнакомая конструкция схемы НЕ пропускается молча, а
// превращается в ошибку. Иначе проверка была бы вакуумно-зелёной: схема
// набирает новое правило, валидатор его не знает, всё зелёное, а контракт
// перестал описывать формат.
package schemacheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// supported — конструкции draft-07, которые валидатор понимает. Всё, чего
// здесь нет, попадёт в unsupported и заставит шаг упасть.
var supported = map[string]bool{
	"$schema": true, "title": true, "description": true,
	"type": true, "enum": true, "const": true, "required": true,
	"properties": true, "additionalProperties": true, "propertyNames": true,
	"items": true, "minItems": true, "maxItems": true,
	"minLength": true, "maxLength": true,
	"minProperties": true, "maxProperties": true,
	"minimum": true, "maximum": true,
	"pattern": true, "oneOf": true, "anyOf": true, "allOf": true,
	"not": true, "if": true, "then": true, "else": true,
}

// literalKeys — ключи, чьи значения НЕ являются схемами. Обход не должен
// проверять ключи внутри них: в `properties` лежат ИМЕНА свойств, а не
// ключевые слова, и без этого разделения охранник объявляет каждое свойство
// неподдержанной конструкцией.
var literalKeys = map[string]bool{
	"enum": true, "const": true, "required": true,
	"examples": true, "default": true,
}

// mapOfSchemas — ключи, чьё значение является картой «имя → схема».
var mapOfSchemas = map[string]bool{
	"properties": true, "patternProperties": true,
	"definitions": true, "$defs": true,
}

// unsupportedConstructs возвращает ключи, которых валидатор не понимает.
// Схемные позиции разделены намеренно: молчаливый пропуск неизвестной
// конструкции хуже явного отказа, но и ложное срабатывание на имя свойства
// делает проверку бесполезной.
func unsupportedConstructs(s interface{}, path string, out map[string]bool) {
	m, ok := s.(map[string]interface{})
	if !ok {
		return
	}
	for k, v := range m {
		here := join(path, k)
		switch {
		case literalKeys[k]:
			continue
		case !supported[k]:
			out[here] = true
			// Всё равно спускаемся: внутри неизвестного ключа могут лежать
			// известные конструкции, и молчать о них нельзя.
			unsupportedConstructs(v, here, out)
		case mapOfSchemas[k]:
			if sub, ok := v.(map[string]interface{}); ok {
				for name, sch := range sub {
					// здесь name — имя свойства, а не ключевое слово
					unsupportedConstructs(sch, join(here, name), out)
				}
			}
		case k == "allOf" || k == "anyOf" || k == "oneOf":
			if list, ok := v.([]interface{}); ok {
				for i, sch := range list {
					unsupportedConstructs(sch, fmt.Sprintf("%s/%d", here, i), out)
				}
			}
		default:
			unsupportedConstructs(v, here, out)
		}
	}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "/" + key
}

// CheckRepo проверяет каталог schemas против содержимого репозитория.
func CheckRepo(repo string) (string, error) {
	schemasDir := filepath.Join(repo, "schemas")
	entries, err := os.ReadDir(schemasDir)
	if err != nil {
		return "", fmt.Errorf("прочитать schemas/: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("schemas/: нет ни одной схемы — проверка сломана, а не прошла")
	}
	sort.Strings(names)

	var problems []string
	var b strings.Builder

	// 1. Схемы обязаны быть валидным JSON и не содержать незнакомых
	//    конструкций. Неизвестная конструкция — ошибка, а не предупреждение.
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(schemasDir, name))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		var doc interface{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			problems = append(problems, fmt.Sprintf("%s: не разбирается как JSON: %v", name, err))
			continue
		}
		unknown := map[string]bool{}
		unsupportedConstructs(doc, "", unknown)
		if len(unknown) > 0 {
			keys := make([]string, 0, len(unknown))
			for k := range unknown {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				problems = append(problems, fmt.Sprintf(
					"%s: конструкция %q не поддержана валидатором — правило не было бы проверено, "+
						"молчаливый пропуск хуже явного отказа", name, k))
			}
		}
	}
	if len(problems) > 0 {
		b.WriteString(fmt.Sprintf("схемы: проверено %d, проблем в самих схемах %d", len(names), len(problems)))
		for _, p := range problems {
			b.WriteString("\n    " + p)
		}
		return b.String(), fmt.Errorf("схемы нечитаемы валидатором: %d", len(problems))
	}
	b.WriteString(fmt.Sprintf("схемы: %d разобраны, конструкций вне подмножества нет", len(names)))

	// 2. Пайплайны из examples/ против pipeline-схемы.
	pb, err := checkDocs(repo, "pipeline.v0.2.schema.json",
		filepath.Join(repo, "examples", "*.yaml"), true)
	if err != nil {
		return b.String(), err
	}
	allProblems := append([]string{}, pb.problems...)
	allUnver := append([]string{}, pb.unver...)
	b.WriteString(fmt.Sprintf("\nпайплайны: %d/%d соответствуют pipeline.v0.2.schema.json", pb.total-pb.bad, pb.total))
	if pb.bad > 0 {
		for _, p := range pb.problems {
			b.WriteString("\n    " + p)
		}
		return b.String(), fmt.Errorf("%d пайплайнов не соответствуют схеме", pb.bad)
	}

	// 3. Манифесты плагинов. Отдельно от пайплайнов: у них другой контракт.
	//
	// Здесь проверяются только манифесты из plugins/. Негативные фикстуры
	// конформности (chatter, spawner) в этот шаг не входят вовсе: они обязаны
	// НЕ соответствовать схеме, а проверяют отказ ядра, поэтому их гоняет шаг
	// conformance, а не этот. Раньше в коде был фильтр isNegativeFixture,
	// который искал их в отчёте, — но паттерны Glob сюда их никогда не
	// приводили, так что фильтр был мёртвым и лишь выглядел обработкой.
	mn, err := checkDocs(repo, "manifest.schema.json",
		filepath.Join(repo, "plugins", "*", "plugin.yaml"), false)
	if err != nil {
		return b.String(), err
	}
	allProblems = append(allProblems, mn.problems...)
	allUnver = append(allUnver, mn.unver...)
	mn2, err := checkDocs(repo, "manifest.schema.json",
		filepath.Join(repo, "plugins", "*", "*", "plugin.yaml"), false)
	if err != nil {
		return b.String(), err
	}
	allProblems = append(allProblems, mn2.problems...)
	allUnver = append(allUnver, mn2.unver...)
	mn.total += mn2.total
	mbad := mn.bad + mn2.bad
	b.WriteString(fmt.Sprintf("\nманифесты: %d/%d соответствуют manifest.schema.json", mn.total-mbad, mn.total))
	for _, p := range allProblems {
		b.WriteString("\n    " + p)
	}
	if mbad > 0 {
		return b.String(), fmt.Errorf("%d манифестов не соответствуют схеме", mbad)
	}
	if len(allUnver) > 0 {
		// Сообщение одно и то же на каждый файл, а правило — свойство схемы.
		// Без дедупликации отчёт в 37 строк говорил бы одно и то же 37 раз.
		b.WriteString("\nнепроверяемое здесь правило (схема корректна для полного валидатора," +
			"\nно движок Go не берёт регулярку; в коде это правило обеспечено процедурно):")
		for _, u := range dedupNotes(allUnver) {
			b.WriteString("\n    " + u)
		}
	}

	// 4. Файлы контракт-тестов плагинов. Отдельная схема, потому что контракт
	//    другой: у plugin.test.yaml своя схема (plugin.test.schema.json), и
	//    неизвестное поле в нём — тоже ошибка, как в манифесте: загрузчик
	//    читает файл с KnownFields(true), а тихо проигнорированный `servcies`
	//    выглядел бы как «сервис есть», когда его нет.
	//
	//    Набор тот же, что у манифестов (plugins/* и plugins/*/*): это то, что
	//    реально лежит в репозитории и что гоняет conformance.
	tn, err := checkDocs(repo, "plugin.test.schema.json",
		filepath.Join(repo, "plugins", "*", "plugin.test.yaml"), false)
	if err != nil {
		return b.String(), err
	}
	tn2, err := checkDocs(repo, "plugin.test.schema.json",
		filepath.Join(repo, "plugins", "*", "*", "plugin.test.yaml"), true)
	if err != nil {
		return b.String(), err
	}
	tn.total += tn2.total
	tbad := tn.bad + tn2.bad
	b.WriteString(fmt.Sprintf("\nфайлы тестов: %d/%d соответствуют plugin.test.schema.json",
		tn.total-tbad, tn.total))
	for _, p := range append(tn.problems, tn2.problems...) {
		b.WriteString("\n    " + p)
	}
	if tbad > 0 {
		return b.String(), fmt.Errorf("%d файлов тестов не соответствуют схеме", tbad)
	}
	for _, u := range dedupNotes(append(tn.unver, tn2.unver...)) {
		b.WriteString("\n    " + u)
	}
	return b.String(), nil
}

// dedupNotes — уникальные сообщения по возрастанию: правило одно, а страдает
// им каждый файл набора.
func dedupNotes(notes []string) []string {
	uniq := map[string]bool{}
	for _, u := range notes {
		uniq[u] = true
	}
	list := make([]string, 0, len(uniq))
	for u := range uniq {
		list = append(list, u)
	}
	sort.Strings(list)
	return list
}

// docBatch — итог одного прогона checkDocs. Возвращается значением, а не
// копится в глобалах: раньше problems и unverifiable были package-level и
// каждый вызов checkDocs их обнулял, поэтому CheckRepo видел детали только
// последней пачки, хотя счётчики вёл по всем. Отчёт при этом выглядел полным.
//
// Негативные фикстуры конформности (chatter, spawner) здесь не обрабатываются:
// они вообще не входят в паттерны этого шага, их гоняет шаг conformance.
// Мёртвый фильтр по подстроке удалён — он выглядел обработкой, но ничего не
// обрабатывал.
type docBatch struct {
	total, bad int
	problems   []string
	unver      []string
}

// checkDocs прогоняет файлы против схемы. isPipeline различает YAML-пайплайны
// (top-level object) и манифесты плагинов.
func checkDocs(repo, schemaName, pattern string, required bool) (docBatch, error) {
	var out docBatch
	raw, err := os.ReadFile(filepath.Join(repo, "schemas", schemaName))
	if err != nil {
		return out, fmt.Errorf("прочитать схему %s: %w", schemaName, err)
	}
	var sch interface{}
	if err := json.Unmarshal(raw, &sch); err != nil {
		return out, fmt.Errorf("%s: %w", schemaName, err)
	}
	files, err := filepath.Glob(pattern)
	if err != nil {
		return out, err
	}
	if len(files) == 0 {
		if required {
			return out, fmt.Errorf("не нашлось файлов под %s — проверка сломана, а не прошла", pattern)
		}
		// Необязательный набор: каталога может просто не быть.
		return out, nil
	}
	for _, f := range files {
		out.total++
		data, err := readYAML(f)
		if err != nil {
			out.bad++
			out.problems = append(out.problems, fmt.Sprintf("%s: %v", rel(repo, f), err))
			continue
		}
		// seen — множество правил, которые валидатор не смог применить. Идёт
		// по всем файлам набора, а не по одному: непроверяемое правило
		// свойство схемы, а не одного файла.
		seen := map[string]bool{}
		if errs := validate(sch, data, "$", seen); len(errs) > 0 {
			out.bad++
			for _, e := range errs {
				out.problems = append(out.problems, fmt.Sprintf("%s: %s", rel(repo, f), e))
			}
		}
		for m := range seen {
			out.unver = append(out.unver, m)
		}
	}
	return out, nil
}

func rel(repo, p string) string {
	r, err := filepath.Rel(repo, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// readYAML читает YAML и превращает карты в map[string]interface{}, иначе
// gopkg.in/yaml.v3 даст map[interface{}]interface{}, и обход схемы не совпадёт.
func readYAML(path string) (interface{}, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var node interface{}
	if err := yaml.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	return normalize(node), nil
}

func normalize(v interface{}) interface{} {
	switch x := v.(type) {
	case map[interface{}]interface{}:
		m := make(map[string]interface{}, len(x))
		for k, val := range x {
			m[fmt.Sprint(k)] = normalize(val)
		}
		return m
	case map[string]interface{}:
		for k, val := range x {
			x[k] = normalize(val)
		}
		return x
	case []interface{}:
		for i, val := range x {
			x[i] = normalize(val)
		}
		return x
	}
	return v
}

// validate — рекурсивная проверка значения против подсхемы.
func validate(sch interface{}, val interface{}, path string, seen map[string]bool) []string {
	switch sc := sch.(type) {
	case bool:
		if !sc {
			return []string{fmt.Sprintf("%s: схема запрещает любое значение", path)}
		}
		return nil
	case map[string]interface{}:
		var out []string
		if t, ok := sc["type"]; ok {
			if !typeMatches(t, val) {
				return []string{fmt.Sprintf("%s: ожидался тип %v, получено %s", path, t, kindOf(val))}
			}
		}
		if e, ok := sc["enum"]; ok {
			if list, ok := e.([]interface{}); ok && !inList(list, val) {
				out = append(out, fmt.Sprintf("%s: значение %v не входит в enum", path, val))
			}
		}
		if c, ok := sc["const"]; ok {
			if !deepEqual(c, val) {
				out = append(out, fmt.Sprintf("%s: ожидалось %v", path, c))
			}
		}
		if r, ok := sc["required"]; ok {
			if names, ok := r.([]interface{}); ok {
				if m, ok := val.(map[string]interface{}); ok {
					for _, n := range names {
						if _, has := m[fmt.Sprint(n)]; !has {
							out = append(out, fmt.Sprintf("%s: нет обязательного поля %v", path, n))
						}
					}
				}
			}
		}
		if p, ok := sc["pattern"]; ok {
			if ps, ok := p.(string); ok && isString(val) {
				re, err := regexp.Compile(ps)
				switch {
				case err != nil:
					// Регулярка корректна для полного валидатора JSON Schema, но
					// движок Go (RE2) её не берёт: например, manifest.json
					// запрещает путь к плагину через отрицательный просмотр
					// вперёд. Молчать об этом нельзя — иначе в отчёте будет
					// написано «правило проверено», а оно не проверено. Правило
					// при этом живо в коде (safeManifestEntry), то есть это не
					// дыра, а граница инструмента.
					msg := fmt.Sprintf("%s: pattern %q не компилируется движком Go — правило здесь не проверено", path, ps)
					seen[msg] = true
				case !re.MatchString(toString(val)):
					out = append(out, fmt.Sprintf("%s: %q не подходит под %s", path, toString(val), ps))
				}
			}
		}
		if sub, ok := sc["items"]; ok {
			if arr, ok := val.([]interface{}); ok {
				for i, item := range arr {
					out = append(out, validate(sub, item, fmt.Sprintf("%s/%d", path, i), seen)...)
				}
			}
		}
		if props, ok := sc["properties"]; ok {
			if m, ok := val.(map[string]interface{}); ok {
				pm, _ := props.(map[string]interface{})
				for k, sub := range pm {
					if v, has := m[k]; has {
						out = append(out, validate(sub, v, path+"."+k, seen)...)
					}
				}
				if ap, ok := sc["additionalProperties"]; ok {
					allowed, isBool := ap.(bool)
					for k := range m {
						if _, known := pm[k]; known {
							continue
						}
						switch {
						case isBool && !allowed:
							out = append(out, fmt.Sprintf("%s: поле %q не описано схемой", path, k))
						case isBool && allowed:
							// разрешено всё
						default:
							out = append(out, validate(ap, m[k], path+"."+k, seen)...)
						}
					}
				}
			}
		}
		if pn, ok := sc["propertyNames"]; ok {
			if m, ok := val.(map[string]interface{}); ok {
				for k := range m {
					out = append(out, validate(pn, k, path+"(имя поля "+k+")", seen)...)
				}
			}
		}
		if n, ok := sc["minLength"]; ok {
			if isString(val) && float64(len([]rune(toString(val)))) < toFloat(n) {
				out = append(out, fmt.Sprintf("%s: короче минимума %v", path, n))
			}
		}
		if n, ok := sc["maxLength"]; ok {
			if isString(val) && float64(len([]rune(toString(val)))) > toFloat(n) {
				out = append(out, fmt.Sprintf("%s: длиннее максимума %v", path, n))
			}
		}
		if n, ok := sc["minItems"]; ok {
			if arr, ok := val.([]interface{}); ok && float64(len(arr)) < toFloat(n) {
				out = append(out, fmt.Sprintf("%s: элементов меньше минимума %v", path, n))
			}
		}
		if n, ok := sc["minProperties"]; ok {
			if m, ok := val.(map[string]interface{}); ok && float64(len(m)) < toFloat(n) {
				out = append(out, fmt.Sprintf("%s: полей меньше минимума %v", path, n))
			}
		}
		if n, ok := sc["minimum"]; ok && isNumber(val) && toFloat(val) < toFloat(n) {
			out = append(out, fmt.Sprintf("%s: %v меньше минимума %v", path, val, n))
		}
		if n, ok := sc["maximum"]; ok && isNumber(val) && toFloat(val) > toFloat(n) {
			out = append(out, fmt.Sprintf("%s: %v больше максимума %v", path, val, n))
		}
		for _, key := range []string{"oneOf", "anyOf", "allOf"} {
			branches, ok := sc[key].([]interface{})
			if !ok {
				continue
			}
			matched := 0
			var branchErrs [][]string
			for _, b := range branches {
				if e := validate(b, val, path, seen); len(e) == 0 {
					matched++
				} else {
					branchErrs = append(branchErrs, e)
				}
			}
			switch key {
			case "oneOf":
				if matched != 1 {
					// Почему не сошлось — самое полезное в сообщении: без ветвей
					// автор схемы видит только счётчик и идёт читать схему сам.
					out = append(out, fmt.Sprintf("%s: oneOf сошёлся ровно с одной ветвью, а сошлось %d%s",
						path, matched, branchErrsHint(branchErrs)))
				}
			case "anyOf":
				if matched == 0 {
					out = append(out, fmt.Sprintf("%s: не подошла ни одна ветвь anyOf%s",
						path, branchErrsHint(branchErrs)))
				}
			case "allOf":
				for i, b := range branches {
					_ = i
					out = append(out, validate(b, val, path, seen)...)
				}
			}
		}
		if nn, ok := sc["not"]; ok {
			if len(validate(nn, val, path, seen)) == 0 {
				out = append(out, fmt.Sprintf("%s: значение подошло под запрещённую схему", path))
			}
		}
		// if/then/else: применяем then только если значение прошло if.
		// response.schema.json выражает через них связки status→output и
		// status→error, поэтому без них схема ответа не проверялась бы вовсе.
		if cond, ok := sc["if"]; ok {
			matched := len(validate(cond, val, path, seen)) == 0
			if matched {
				if th, ok := sc["then"]; ok {
					out = append(out, validate(th, val, path+"(then)", seen)...)
				}
			} else if el, ok := sc["else"]; ok {
				out = append(out, validate(el, val, path+"(else)", seen)...)
			}
		}
		return out
	}
	return nil
}

func typeMatches(t interface{}, v interface{}) bool {
	names, ok := t.(string)
	if !ok {
		if list, ok := t.([]interface{}); ok {
			for _, n := range list {
				if typeMatches(n, v) {
					return true
				}
			}
			return false
		}
		return true
	}
	switch names {
	case "object":
		_, ok := v.(map[string]interface{})
		return ok
	case "array":
		_, ok := v.([]interface{})
		return ok
	case "string":
		return isString(v)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		return isNumber(v)
	case "integer":
		if !isNumber(v) {
			return false
		}
		f := toFloat(v)
		return f == float64(int64(f))
	case "null":
		return v == nil
	}
	return true
}

func inList(list []interface{}, v interface{}) bool {
	for _, item := range list {
		if deepEqual(item, v) {
			return true
		}
	}
	return false
}

func deepEqual(a, b interface{}) bool {
	switch x := a.(type) {
	case string:
		s, ok := b.(string)
		return ok && x == s
	case bool:
		t, ok := b.(bool)
		return ok && x == t
	case float64:
		return isNumber(b) && toFloat(b) == x
	case []interface{}:
		y, ok := b.([]interface{})
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !deepEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]interface{}:
		y, ok := b.(map[string]interface{})
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, has := y[k]
			if !has || !deepEqual(v, w) {
				return false
			}
		}
		return true
	}
	return a == nil && b == nil
}

func isString(v interface{}) bool { _, ok := v.(string); return ok }
func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
func isNumber(v interface{}) bool {
	switch v.(type) {
	case int, int64, float64:
		return true
	}
	return false
}
func toFloat(v interface{}) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return 0
}
func kindOf(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case int, int64, float64:
		return "number"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// branchErrsHint — что именно не сошлось в ветвях oneOf/anyOf. Ограничение по
// длине: сообщение уходит в CLI и в JSON, простыня из пяти ветвей там мешает.
func branchErrsHint(branchErrs [][]string) string {
	if len(branchErrs) == 0 {
		return ""
	}
	const perBranch, maxBranches = 2, 3
	var parts []string
	for i, errs := range branchErrs {
		if i == maxBranches {
			parts = append(parts, fmt.Sprintf("… ещё ветвей: %d", len(branchErrs)-maxBranches))
			break
		}
		cut := errs
		if len(cut) > perBranch {
			cut = cut[:perBranch]
		}
		parts = append(parts, strings.Join(cut, "; "))
	}
	return " (ветви: " + strings.Join(parts, " | ") + ")"
}
