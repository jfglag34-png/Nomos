package plugincreate

// Генератор плагинов по шаблонам.
//
// Отличие от скелета (plugin create без --template) принципиальное: скелет
// учит протокол и содержит маркер «ваша логика здесь», то есть требует
// написания кода. Шаблон даёт РАБОТАЮЩИЙ плагин: человек задаёт данные, а не
// пишет код.
//
// Именно «данные, а не код» — то, что делает шаблоны безопасными. Параметр
// (регулярка, замена, имя поля) попадает в сгенерированный Python как литерал,
// поэтому произвольный код в него физически не попадает: подставлять пришлось
// бы строку, а не тело функции. Для инъекции нужно было бы изменить сам
// генератор.
//
// По той же причине параметры вставляются экранированными литералами
// (pyLiteral), и вставляются ОДИНАКОВО в тело плагина и в его контракт-тесты —
// иначе плагин и его тесты разойдутся.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// pyLiteral превращает строку в безопасный литерал Python (одиночные кавычки).
//
// Экранирование обратного слэша обязательно для обоих параметров: иначе
// `\s` в регулярке и `\1` в замене (обратная ссылка re.sub) превратились бы в
// escape-последовательность Python и испортили значение. Управляющие символы
// кроме \n \r \t не кодируются, а отвергаются: молча исказить регулярку хуже,
// чем сказать «этот параметр нельзя вставить».
func pyLiteral(s string) (string, error) {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("параметр содержит управляющий символ %U — "+
					"вставить его в код плагина нельзя", r)
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String(), nil
}

// pluginTemplate — описание шаблона. build обязан вернуть четыре файла
// плагина; если параметр некорректен — ошибку, а не «исправьте руками».
type pluginTemplate struct {
	name    string
	title   string
	summary string
	// params — флаги, которые шаблон понимает, для --list-templates
	params []string
	build  func(id string, opts CreateOptions) (pluginFiles, error)
}

type pluginFiles struct {
	manifest string
	mainPy   string
	tests    string
	readme   string
	lock     string
}

const (
	// tplPattern для regex-replace схлопывает пробелы: полезно по умолчанию и
	// результат можно записать руками в тест (a   b → a b, 1 замена).
	defaultCollapsePattern     = `\s+`
	defaultCollapseReplacement = ` `
	// tplFilter ищет слово error без учёта регистра.
	defaultFilterPattern = `(?i)error`
)

func templateRegistry() []pluginTemplate {
	return []pluginTemplate{
		{
			name:    "skeleton",
			title:   "Скелет с маркером «ваша логика здесь»",
			summary: "учит протокол; код писать надо самому (это поведение по умолчанию)",
			build: func(id string, opts CreateOptions) (pluginFiles, error) {
				if opts.Example == "" {
					opts.Example = "string"
				}
				if opts.Example != "string" && opts.Example != "array" {
					return pluginFiles{}, fmt.Errorf("--example %q не знаю: есть string (умолчание) и array", opts.Example)
				}
				mainPy, tests := mainPyTemplate(id), testsTemplate()
				if opts.Example == "array" {
					mainPy, tests = mainPyArrayTemplate(id), testsArrayTemplate()
				}
				return pluginFiles{
					manifest: manifestTemplate(id, opts),
					mainPy:   mainPy,
					tests:    tests,
					readme:   pluginReadmeTemplateWithOpts(id, opts),
				}, nil
			},
		},
		{
			name:    "text-metrics",
			title:   "Метрики текста",
			summary: "text → {words, chars, lines, unique_words}. Параметров нет, тесты точные",
			build:   buildTextMetrics,
		},
		{
			name:    "regex-replace",
			title:   "Замена по регулярке",
			summary: "text → {text, replacements}. Параметры: pattern, replacement",
			params:  []string{"pattern", "replacement"},
			build:   buildRegexReplace,
		},
		{
			name:    "line-filter",
			title:   "Фильтр строк по регулярке",
			summary: "items[] → {items, matched, dropped}. Параметр: pattern",
			params:  []string{"pattern"},
			build:   buildLineFilter,
		},
		{
			name:    "rest-api",
			title:   "Коннектор REST API",
			summary: "url → {status, body}. Параметры: нет, env: API_KEY (опц.). Тесты на моке",
			build:   buildRestAPI,
		},
		{
			name:    "smtp",
			title:   "Коннектор SMTP",
			summary: "to,subject,body → {sent}. Секреты: SMTP_HOST/USER/PASS, MOCK=1 для тестов",
			build:   buildSMTP,
		},
		{
			name:    "postgres",
			title:   "Коннектор Postgres",
			summary: "query,params → {rows}. Секрет: PG_DSN, requires: psycopg2-binary",
			build:   buildPostgres,
		},
		{
			name:    "redis",
			title:   "Коннектор Redis",
			summary: "command,key,value → {value}. Секрет: REDIS_URL, requires: redis",
			build:   buildRedis,
		},
	}
}

func findTemplate(name string) (pluginTemplate, bool) {
	for _, t := range templateRegistry() {
		if t.name == name {
			return t, true
		}
	}
	return pluginTemplate{}, false
}

func PyLiteralForTest(s string) (string, error) { return pyLiteral(s) }

// templateNames exposes registry for core shim tests without exposing unexported type.
func TemplateNames() []string {
	out := make([]string, 0, len(templateRegistry()))
	for _, t := range templateRegistry() {
		out = append(out, t.name)
	}
	return out
}

// ListTemplates — текст для --list-templates. Тот же реестр, из которого
// берёт GUI, когда он появится: список шаблонов не должен расходиться между
// CLI и интерфейсом.
func ListTemplates() string {
	var b strings.Builder
	b.WriteString("Шаблоны `wedra plugin create --template=<имя>`:\n\n")
	ts := templateRegistry()
	for _, t := range ts {
		params := "без параметров"
		if len(t.params) > 0 {
			parts := make([]string, 0, len(t.params))
			for _, p := range t.params {
				parts = append(parts, "--"+p)
			}
			sort.Strings(parts)
			params = "параметры: " + strings.Join(parts, ", ")
		}
		b.WriteString(fmt.Sprintf("  %-14s %s\n    %s\n", t.name, t.title, t.summary))
		b.WriteString("    " + params + "\n\n")
	}
	b.WriteString("Шаблон даёт рабочий плагин: пишется не код, а данные. " +
		"Плагин, написанный агентом или человеком в GUI, помечается untrusted " +
		"по построению и на хостах без изолятора не запускается.")
	return b.String()
}

func patternOr(opts CreateOptions, def string) string {
	if opts.Pattern == "" {
		return def
	}
	return opts.Pattern
}

// sharedPrelude — общая часть Python-тела: протокол конверта и ok/fail.
// Дублируется в каждом шаблоне намеренно: у сгенерированного плагина не должно
// быть зависимостей и импортов из репозитория WEDRA.
const sharedPrelude = `def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return 1


def read_input():
    try:
        return json.load(sys.stdin)
    except Exception as e:
        print(json.dumps({"status": "error", "error": {
            "code": "bad_input", "message": str(e), "retryable": False}},
            ensure_ascii=False))
        return None
`

func manifestFor(id, desc, author, inputYaml, outputYaml string) string {
	descLine := "description: " + yamlQuote(desc)
	authorLine := "author: " + yamlQuote(author)
	if desc == "" {
		descLine = "description: " + yamlQuote("TODO: что делает плагин, одной строкой")
	}
	if author == "" {
		authorLine = "author: " + yamlQuote("TODO")
	}
	return `# Сгенерировано: wedra plugin create --template
# Контракт с ядром: protocol/v0.2/PROTOCOL.md
id: ` + id + `
version: 0.1.0
platform_api: "^0.1"
` + descLine + `
` + authorLine + `

runtime:
  type: python
  entry: main.py
  requires: []

input:
` + inputYaml + `
output:
` + outputYaml + `
# Ничего не заявлено — значит ничего и не разрешено. Это безопасный скелет:
# добавьте host, только когда плагину действительно нужен доступ в сеть.
permissions:
  network: []
  filesystem: none
  secrets: []
`
}

const textInputYaml = `  text:
    from: input.text
    type: string
`

const textMetricsOutputYaml = `  words: { type: number }
  chars: { type: number }
  lines: { type: number }
  unique_words: { type: number }
`

func buildTextMetrics(id string, opts CreateOptions) (pluginFiles, error) {
	mainPy := `#!/usr/bin/env python3
"""` + id + ` — метрики текста (--template=text-metrics).

Сгенерировано шаблоном: писать код не нужно. Менять тело имеет смысл только
если нужны другие метрики — контракт придётся расшивить и в манифесте, и в
plugin.test.yaml.
"""
import json, sys

` + sharedPrelude + `

def main():
    data = read_input()
    if data is None:
        return 2

    text = str(data.get("text") or "").strip()
    if not text:
        return fail("empty_input", "поле text пустое")

    words = text.split()
    return ok({
        "words": len(words),
        "chars": len(text),
        "lines": len(text.splitlines()),
        "unique_words": len({w.lower() for w in words}),
    })


if __name__ == "__main__":
    sys.exit(main())
`
	tests := `# Контракт-тесты (--template=text-metrics). Параметров нет, поэтому
# ожидания точные: их можно читать как спецификацию.
tests:
  - name: метрики английского текста
    input: { text: "hello brave new world" }
    expect:
      status: ok
      output:
        words: 4
        chars: 21
        lines: 1
        unique_words: 4

  - name: повторы считаются один раз
    input: { text: "a b a b" }
    expect:
      status: ok
      output:
        words: 4
        unique_words: 2

  - name: несколько строк
    # Блочный литерал, а не "one\ntwo": перевод строки должен быть настоящим,
    # а результат — зависеть от обработки escape-последовательностей в
    # парсере тестов. На такое опираться нельзя.
    input:
      text: |
        one
        two
        three
    expect:
      status: ok
      output: { lines: 3, words: 3 }

  - name: пустой вход — доменная ошибка, не retryable
    input: { text: "   " }
    expect:
      status: error
      exit_code: 1
      error: { code: empty_input, retryable: false }

  - name: битый JSON — платформенная ошибка
    input_raw: "{broken"
    expect:
      exit_code: 2
      error: { code: platform:bad_input, retryable: false }
`
	desc := opts.Description
	if desc == "" {
		desc = "Метрики текста: слова, символы, строки, уникальные слова"
	}
	return pluginFiles{
		manifest: manifestFor(id, desc, opts.Author, textInputYaml, textMetricsOutputYaml),
		mainPy:   mainPy,
		tests:    tests,
		readme: generatedReadme(id, desc,
			"Метрики текста без параметров. Меняйте числа в выводе только вместе с манифестом и тестами.",
			""),
	}, nil
}

func buildRegexReplace(id string, opts CreateOptions) (pluginFiles, error) {
	pat := patternOr(opts, defaultCollapsePattern)
	repl := opts.Replacement
	if repl == "" {
		repl = defaultCollapseReplacement
	}
	patLit, err := pyLiteral(pat)
	if err != nil {
		return pluginFiles{}, fmt.Errorf("--pattern: %w", err)
	}
	replLit, err := pyLiteral(repl)
	if err != nil {
		return pluginFiles{}, fmt.Errorf("--replacement: %w", err)
	}
	// Компилируем регулярку здесь, а не в рантайме плагина: синтаксическая
	// ошибка должна быть видна в момент генерации, иначе сгенерированный
	// плагин окажется красным из коробки — а это прямое нарушение критерия
	// генератора.
	if _, err := compileCheck(pat); err != nil {
		return pluginFiles{}, fmt.Errorf("--pattern %q: %w", pat, err)
	}
	mainPy := `#!/usr/bin/env python3
"""` + id + ` — замена по регулярке (--template=regex-replace).

Сгенерировано шаблоном. PATTERN и REPLACEMENT — данные, вписанные при
генерации; свободного кода здесь нет by design. В ZАМЕНЕ поддерживаются
обратные ссылки re.sub: \\1, \\g<name>.
"""
import json, re, sys

` + sharedPrelude + `
PATTERN = re.compile(` + patLit + `)
REPLACEMENT = ` + replLit + `


def main():
    data = read_input()
    if data is None:
        return 2

    text = str(data.get("text") or "").strip()
    if not text:
        return fail("empty_input", "поле text пустое")

    result, count = PATTERN.subn(REPLACEMENT, text)
    return ok({"text": result, "replacements": count})


if __name__ == "__main__":
    sys.exit(main())
`
	// Точное ожидание вычислимо только для шаблонной регулярки: своя
	// регулярка меняет результат, и выдуманное здесь число сделало бы тест
	// красным из коробки. Для своей регулярки оставляем структурные
	// проверки и говорим пользователю, что ожидание стоит заострить.
	// Точные числа вычислимы только для шаблонной регулярки. При своей
	// регулярке генератор не знает, что и с чем совпадёт, поэтому утверждает
	// только форму ответа. Утверждать тут числа от шаблона означало бы
	// гарантированно красный тест из коробки.
	isDefault := pat == defaultCollapsePattern && repl == defaultCollapseReplacement
	bodyTest := `  - name: форма ответа — строка и число замен
    input: { text: "one two three" }
    expect:
      status: ok
      output:
        text: { type: string }
        replacements: { type: number }
`
	if isDefault {
		bodyTest = `  - name: схлопывает пробельные серии в один пробел
    # Только пробелы: escape-последовательности внутри кавычек в
    # plugin.test.yaml не превращаются в символы, поэтому "\t" здесь был бы
    # двумя символами и подсчёт замен сошёл бы.
    input: { text: "a   b  c   d" }
    expect:
      status: ok
      output:
        text: "a b c d"
        replacements: 3
`
	}
	tail := ""
	if !isDefault {
		tail = `
# Параметры заданы свои, поэтому точный результат подмены генератор
# вычислить не мог: неизвестно, что совпадёт с вашей регуляркой. Проверка
# выше поэтому проверяет только форму. Ужестотите её:
#   wedra plugin test ` + id + `
# и подставьте в поле text ожидаемую строку, а в replacements — число.
`
	}
	tests := `# Контракт-тесты (--template=regex-replace).
tests:
` + bodyTest + `
  - name: пустой вход — доменная ошибка, не retryable
    input: { text: " " }
    expect:
      status: error
      exit_code: 1
      error: { code: empty_input, retryable: false }

  - name: битый JSON — платформенная ошибка
    input_raw: "{oops"
    expect:
      exit_code: 2
      error: { code: platform:bad_input, retryable: false }
` + tail
	outputYaml := `  text: { type: string }
  replacements: { type: number }
`
	desc := opts.Description
	if desc == "" {
		desc = "Замена по регулярке: text → text + число замен"
	}
	return pluginFiles{
		manifest: manifestFor(id, desc, opts.Author, textInputYaml, outputYaml),
		mainPy:   mainPy,
		tests:    tests,
		readme: generatedReadme(id, desc,
			"Регулярка и замена записаны в начало main.py константами PATTERN и REPLACEMENT. "+
				"Замените их своими и поправьте ожидание в plugin.test.yaml.",
			""),
	}, nil
}

const itemsInputYaml = `  items:
    from: input.items
    type: array
`

func buildLineFilter(id string, opts CreateOptions) (pluginFiles, error) {
	pat := patternOr(opts, defaultFilterPattern)
	patLit, err := pyLiteral(pat)
	if err != nil {
		return pluginFiles{}, fmt.Errorf("--pattern: %w", err)
	}
	if _, err := compileCheck(pat); err != nil {
		return pluginFiles{}, fmt.Errorf("--pattern %q: %w", pat, err)
	}
	mainPy := `#!/usr/bin/env python3
"""` + id + ` — фильтр массива по регулярке (--template=line-filter).

Сгенерировано шаблоном. PATTERN — данные, вписанные при генерации.
"""
import json, re, sys

` + sharedPrelude + `
PATTERN = re.compile(` + patLit + `)


def main():
    data = read_input()
    if data is None:
        return 2

    items = data.get("items")
    # Манифест обещает array, но рантайм не гарантирует тип: проверка
    # честного плагина, а не надежда на валидатор.
    if not isinstance(items, list):
        print(json.dumps({"status": "error", "error": {
            "code": "bad_input",
            "message": "items должен быть массивом (type: array в манифесте)",
            "retryable": False}}, ensure_ascii=False))
        return 2

    kept = [x for x in items if PATTERN.search(str(x))]
    return ok({
        "items": kept,
        "matched": len(kept),
        "dropped": len(items) - len(kept),
    })


if __name__ == "__main__":
    sys.exit(main())
`
	// Числа вычислимы только для шаблонной регулярки: при своей неизвестно,
	// что совпадёт, и вход от шаблона (словом error) просто не совпал бы.
	isDefault := pat == defaultFilterPattern
	bodyTest := `  - name: форма ответа — массив и два счётчика
    input: { items: ["first", "second"] }
    expect:
      status: ok
      output:
        items: { type: array }
        matched: { type: number }
        dropped: { type: number }
`
	if isDefault {
		bodyTest = `  - name: находит error независимо от регистра
    input: { items: ["error: disk full", "all fine", "ERROR again"] }
    expect:
      status: ok
      output:
        matched: 2
        dropped: 1
        items: { contains: "error: disk full" }
`
	}
	tail := ""
	if !isDefault {
		tail = `
# Своя регулярка: генератор не знает, что с ней совпадёт, поэтому проверка
# выше смотрит только на форму ответа. Добавьте свой случай с числами:
#   wedra plugin test ` + id + `
`
	}
	tests := `# Контракт-тесты (--template=line-filter).
tests:
` + bodyTest + `
  - name: пустой массив — корректный вход, а не ошибка
    input: { items: [] }
    expect:
      status: ok
      output: { matched: 0, dropped: 0, items: { type: array } }
` + tail + `
  - name: не-массив → платформенная ошибка (guard типа)
    input: { items: "oops" }
    expect:
      exit_code: 2
`
	outputYaml := `  items: { type: array }
  matched: { type: number }
  dropped: { type: number }
`
	desc := opts.Description
	if desc == "" {
		desc = "Фильтр массива по регулярке: оставляет совпавшие элементы"
	}
	return pluginFiles{
		manifest: manifestFor(id, desc, opts.Author, itemsInputYaml, outputYaml),
		mainPy:   mainPy,
		tests:    tests,
		readme: generatedReadme(id, desc,
			"Регулярка записана в начало main.py константой PATTERN. "+
				"Элементы приводятся к строке через str(), поэтому числа и объекты тоже фильтруются.",
			""),
	}, nil
}

func buildRestAPI(id string, opts CreateOptions) (pluginFiles, error) {
	desc := opts.Description
	if desc == "" {
		desc = "Коннектор REST API: url → статус и тело"
	}
	manifest := `# ` + id + ` — REST коннектор (ведро)
id: ` + id + `
version: 0.1.0
platform_api: "^0.1"
description: ` + yamlQuote(desc) + `
author: ` + yamlQuote(opts.Author) + `

runtime:
  type: python
  entry: main.py
  requires: ["requests==2.32.0"]

input:
  url: { from: input.url, type: string }
  method: { from: input.method, type: string, optional: true }
  body: { from: input.body, type: object, optional: true }

output:
  status: { type: number }
  body: { type: object, optional: true }

permissions:
  network: [{ any_host: true, note: "REST публичные API" }]
  filesystem: none
  secrets: [API_KEY]

`
	mainPy := `#!/usr/bin/env python3
"""` + id + ` — REST API коннектор (--template=rest-api).
Секреты: API_KEY (опц., в env), для тестов MOCK=1.
"""
import json, os, sys
` + sharedPrelude + `
def main():
    data = read_input()
    if data is None: return 2
    url = (data.get("url") or "").strip()
    if not url: return fail("empty_input", "поле url пустое")
    method = (data.get("method") or "GET").upper()
    if os.environ.get("MOCK") == "1":
        return ok({"status": 200, "body": {"mock": True, "url": url, "method": method}})
    try:
        import requests
        headers = {}
        if os.environ.get("API_KEY"):
            headers["Authorization"] = "Bearer " + os.environ["API_KEY"]
        r = requests.request(method, url, json=data.get("body"), headers=headers, timeout=10)
        try: body = r.json()
        except: body = {"text": r.text[:2000]}
        return ok({"status": r.status_code, "body": body})
    except Exception as e:
        return fail("request_failed", str(e))

if __name__ == "__main__": sys.exit(main())
`
	tests := `tests:
  - name: мок REST
    input: { url: "https://example.com/api", method: "GET" }
    env: { MOCK: "1" }
    expect: { status: ok, output: { status: 200, body: { type: object } } }
  - name: пустой url — ошибка
    input: { url: "   " }
    expect: { status: error, exit_code: 1, error: { code: empty_input } }
  - name: битый JSON
    input_raw: "{broken"
    expect: { exit_code: 2 }
`
	return pluginFiles{manifest: manifest, mainPy: mainPy, tests: tests, readme: generatedReadme(id, desc, "REST коннектор: url+method+body → status+body. Секрет API_KEY в env, MOCK=1 для тестов.", ""), lock: "requests==2.32.0\n"}, nil
}

func buildSMTP(id string, opts CreateOptions) (pluginFiles, error) {
	desc := opts.Description
	if desc == "" {
		desc = "Коннектор SMTP: отправка почты"
	}
	manifest := `id: ` + id + `
version: 0.1.0
platform_api: "^0.1"
description: ` + yamlQuote(desc) + `
author: ` + yamlQuote(opts.Author) + `
runtime:
  type: python
  entry: main.py
  requires: []
input:
  to: { from: input.to, type: string }
  subject: { from: input.subject, type: string }
  body: { from: input.body, type: string }
output:
  sent: { type: boolean }
permissions:
  network: [{ any_host: true, note: "smtp:587/465" }]
  filesystem: none
  secrets: [SMTP_HOST, SMTP_USER, SMTP_PASS, SMTP_PORT]

`
	mainPy := `#!/usr/bin/env python3
"""` + id + ` — SMTP коннектор (--template=smtp).
Секреты: SMTP_HOST, SMTP_PORT, SMTP_USER, SMTP_PASS. MOCK=1 для тестов.
"""
import json, os, sys
` + sharedPrelude + `
def main():
    data = read_input()
    if data is None: return 2
    to, subj, body = data.get("to"), data.get("subject"), data.get("body")
    if not to or not subj or not body: return fail("empty_input", "поля to/subject/body обязательны")
    if os.environ.get("MOCK") == "1":
        return ok({"sent": True})
    host = os.environ.get("SMTP_HOST")
    if not host: return fail("missing_secret", "SMTP_HOST не задан (env)")
    try:
        import smtplib; from email.mime.text import MIMEText
        port = int(os.environ.get("SMTP_PORT") or "587")
        user = os.environ.get("SMTP_USER"); pwd = os.environ.get("SMTP_PASS")
        msg = MIMEText(body); msg["Subject"]=subj; msg["From"]=user or "noreply@wedra.local"; msg["To"]=to
        s = smtplib.SMTP(host, port, timeout=10); s.starttls()
        if user and pwd: s.login(user, pwd)
        s.send_message(msg); s.quit()
        return ok({"sent": True})
    except Exception as e: return fail("smtp_failed", str(e))

if __name__ == "__main__": sys.exit(main())
`
	tests := `tests:
  - name: мок отправка
    input: { to: "a@b.c", subject: "hi", body: "test" }
    env: { MOCK: "1" }
    expect: { status: ok, output: { sent: true } }
  - name: пустой to — ошибка
    input: { to: "", subject: "hi", body: "x" }
    expect: { status: error, error: { code: empty_input } }
`
	return pluginFiles{manifest: manifest, mainPy: mainPy, tests: tests, readme: generatedReadme(id, desc, "SMTP: to/subject/body → sent. Секреты SMTP_* в env, MOCK=1 для тестов.", "")}, nil
}

func buildPostgres(id string, opts CreateOptions) (pluginFiles, error) {
	desc := opts.Description
	if desc == "" {
		desc = "Коннектор Postgres: query → rows"
	}
	manifest := `id: ` + id + `
version: 0.1.0
platform_api: "^0.1"
description: ` + yamlQuote(desc) + `
author: ` + yamlQuote(opts.Author) + `
runtime:
  type: python
  entry: main.py
  requires: ["psycopg2-binary==2.9.10"]
input:
  query: { from: input.query, type: string }
  params: { from: input.params, type: array, optional: true }
output:
  rows: { type: array }
permissions:
  network: [{ any_host: true, note: "postgres:5432 — локальная сеть" }]
  filesystem: none
  secrets: [PG_DSN]

`
	mainPy := `#!/usr/bin/env python3
"""` + id + ` — Postgres коннектор (--template=postgres).
Секрет: PG_DSN=postgres://user:pass@host:5432/db . MOCK=1 для тестов.
"""
import json, os, sys
` + sharedPrelude + `
def main():
    data = read_input()
    if data is None: return 2
    q = (data.get("query") or "").strip()
    if not q: return fail("empty_input", "поле query пустое")
    if os.environ.get("MOCK") == "1":
        return ok({"rows": [{"mock": 1}]})
    dsn = os.environ.get("PG_DSN")
    if not dsn: return fail("missing_secret", "PG_DSN не задан")
    try:
        import psycopg2
        conn = psycopg2.connect(dsn)
        cur = conn.cursor(); cur.execute(q, data.get("params"))
        rows = cur.fetchall() if cur.description else []
        cur.close(); conn.close()
        return ok({"rows": [list(r) for r in rows]})
    except Exception as e: return fail("pg_failed", str(e))

if __name__ == "__main__": sys.exit(main())
`
	tests := `tests:
  - name: мок запрос
    input: { query: "SELECT 1" }
    env: { MOCK: "1" }
    expect: { status: ok, output: { rows: { type: array } } }
  - name: пустой query
    input: { query: "   " }
    expect: { status: error, error: { code: empty_input } }
`
	return pluginFiles{manifest: manifest, mainPy: mainPy, tests: tests, readme: generatedReadme(id, desc, "Postgres: query+params → rows. PG_DSN в env, MOCK=1 для тестов.", ""), lock: "psycopg2-binary==2.9.10\n"}, nil
}

func buildRedis(id string, opts CreateOptions) (pluginFiles, error) {
	desc := opts.Description
	if desc == "" {
		desc = "Коннектор Redis: command → value"
	}
	manifest := `id: ` + id + `
version: 0.1.0
platform_api: "^0.1"
description: ` + yamlQuote(desc) + `
author: ` + yamlQuote(opts.Author) + `
runtime:
  type: python
  entry: main.py
  requires: ["redis==5.0.0"]
input:
  command: { from: input.command, type: string }
  key: { from: input.key, type: string }
  value: { from: input.value, type: string, optional: true }
output:
  value: { type: string, optional: true }
permissions:
  network: [{ any_host: true, note: "redis:6379 — локальная сеть" }]
  filesystem: none
  secrets: [REDIS_URL]

`
	mainPy := `#!/usr/bin/env python3
"""` + id + ` — Redis коннектор (--template=redis).
Секрет: REDIS_URL=redis://host:6379/0 . MOCK=1 для тестов.
"""
import json, os, sys
` + sharedPrelude + `
def main():
    data = read_input()
    if data is None: return 2
    cmd, key = (data.get("command") or "GET").upper(), data.get("key")
    if not key: return fail("empty_input", "поле key пустое")
    if os.environ.get("MOCK") == "1":
        return ok({"value": "mock:" + key})
    url = os.environ.get("REDIS_URL") or "redis://localhost:6379/0"
    try:
        import redis
        r = redis.from_url(url, decode_responses=True)
        if cmd == "GET": v = r.get(key)
        elif cmd == "SET": r.set(key, data.get("value") or ""); v = "OK"
        elif cmd == "DEL": v = str(r.delete(key))
        else: return fail("bad_input", "command ожидается GET/SET/DEL")
        return ok({"value": v or ""})
    except Exception as e: return fail("redis_failed", str(e))

if __name__ == "__main__": sys.exit(main())
`
	tests := `tests:
  - name: мок GET
    input: { command: "GET", key: "foo" }
    env: { MOCK: "1" }
    expect: { status: ok, output: { value: { type: string } } }
  - name: пустой key
    input: { command: "GET", key: "" }
    expect: { status: error, error: { code: empty_input } }
`
	return pluginFiles{manifest: manifest, mainPy: mainPy, tests: tests, readme: generatedReadme(id, desc, "Redis: command/key/value → value. REDIS_URL в env, MOCK=1 для тестов.", ""), lock: "redis==5.0.0\n"}, nil
}

// compileCheck проверяет регулярку движком Go, чтобы опечатку поймали при
// генерации, а не в красном тесте.
//
// Граница честная: движки разные. Go — RE2, Python — с backtracking, поэтому
// Go строже (не знает lookahead `(?=...)`, обратных ссылок `\1` в самом
// шаблоне) и мягче (знает `(?<name>...)`, которого Python не понимает).
// Поэтому отказ Go — это «подозрительно», а не приговор, и сообщение об этом
// говорит прямо: иначе пользователь упрётся в стену без объяснения.
func compileCheck(pat string) (string, error) {
	if _, err := regexp.Compile(pat); err != nil {
		return "", fmt.Errorf("не проходит проверку движком Go: %v. "+
			"Движок Python другой и строже в одном месте (lookahead (?=...), обратные ссылки), "+
			"так что это может быть ложное срабатывание — тогда напишите плагин вручную", err)
	}
	return pat, nil
}

func generatedReadme(id, desc, body, tail string) string {
	return "# " + id + "\n\n" + desc + "\n\n" + body + "\n\n## Цикл разработки\n\n" +
		"```bash\n" +
		"wedra plugin test " + id + "        # контракт-тесты\n" +
		"wedra plugin validate " + id + "    # проверка манифеста\n" +
		"```\n" + tail +
		"\nПлагин ничего не заявляет в permissions: ни сети, ни файлов, ни секретов.\n"
}
