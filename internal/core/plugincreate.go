package core

// tool plugin create <path> — генератор скелета плагина (ТЗ v2 §6.1).
// Критерий качества генератора: свежесозданный плагин СРАЗУ проходит
// plugin validate и plugin test — скелет является проигрываемым примером
// протокола, а не заготовкой-черновиком.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CreateOptions — опциональные поля скелета (M5, тестер №2: author/description
// через флаги, чтобы скелет не выглядел «недоделкой» с TODO; тестер №3: пример
// типа порта, т.к. string-овый скелет не учил array — см. --example).
type CreateOptions struct {
	Author      string
	Description string
	Example     string // "" | "string" (умолчание) | "array"

	// Template — имя шаблона. "" и "skeleton" ведут в скелет с маркером
	// «ваша логика здесь». Остальные шаблоны дают рабочий плагин: задаются
	// ДАННЫЕ, а не код (см. plugincreate_templates.go).
	Template string
	// Pattern / Replacement — параметры шаблонов regex-replace и line-filter.
	// Пусто значит «взять умолчание шаблона».
	Pattern     string
	Replacement string
}

// ParseCreateArgs разбирает аргументы `plugin create`: путь и флаги
// (--author, --description) В ЛЮБОМ порядке — стандартный flag-пакет Go
// обрывается на первом позиционном, а тестер №2 интуитивно пишет флаги после пути:
//
//	tool plugin create plugins/url_checker --author me --description "Checks URLs"
//
// listRequested — пользователь попросил перечень шаблонов, а не создание.
func ParseCreateArgs(args []string) (dir string, opts CreateOptions, listRequested bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--list-templates":
			listRequested = true
		case a == "--author" || a == "--description" || a == "--example" ||
			a == "--template" || a == "--pattern" || a == "--replacement":
			if i+1 >= len(args) {
				return "", opts, listRequested, fmt.Errorf("флагу %s нужно значение", a)
			}
			i++
			switch a {
			case "--author":
				opts.Author = args[i]
			case "--description":
				opts.Description = args[i]
			case "--template":
				opts.Template = args[i]
			case "--pattern":
				opts.Pattern = args[i]
			case "--replacement":
				opts.Replacement = args[i]
			default:
				opts.Example = args[i]
			}
		case strings.HasPrefix(a, "--author="):
			opts.Author = strings.TrimPrefix(a, "--author=")
		case strings.HasPrefix(a, "--description="):
			opts.Description = strings.TrimPrefix(a, "--description=")
		case strings.HasPrefix(a, "--example="):
			opts.Example = strings.TrimPrefix(a, "--example=")
		case strings.HasPrefix(a, "--template="):
			opts.Template = strings.TrimPrefix(a, "--template=")
		case strings.HasPrefix(a, "--pattern="):
			opts.Pattern = strings.TrimPrefix(a, "--pattern=")
		case strings.HasPrefix(a, "--replacement="):
			opts.Replacement = strings.TrimPrefix(a, "--replacement=")
		case strings.HasPrefix(a, "-"):
			return "", opts, listRequested, fmt.Errorf("неизвестный флаг %q (есть --template/--pattern/--replacement/--author/--description/--example; перечень: --list-templates)", a)
		default:
			if dir != "" {
				return "", opts, listRequested, fmt.Errorf("лишний позиционный аргумент %q — путь один", a)
			}
			dir = a
		}
	}
	if listRequested {
		return dir, opts, true, nil
	}
	if dir == "" {
		return "", opts, listRequested, fmt.Errorf("не указан путь плагина")
	}
	return dir, opts, listRequested, nil
}

var pluginIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// CreatePlugin создаёт скелет плагина в dir (id = имя папки).
// Отказывается перезаписывать непустую папку.
func CreatePlugin(dir string) (string, error) {
	return CreatePluginWith(dir, CreateOptions{})
}

func CreatePluginWith(dir string, opts CreateOptions) (string, error) {
	if opts.Example == "" {
		opts.Example = "string"
	}
	if opts.Example != "string" && opts.Example != "array" {
		return "", fmt.Errorf("--example %q не знаю: есть string (умолчание) и array", opts.Example)
	}
	tplName := opts.Template
	if tplName == "" {
		tplName = "skeleton"
	}
	tpl, ok := findTemplate(tplName)
	if !ok {
		return "", fmt.Errorf("шаблона %q нет: %s", tplName, templateNames())
	}
	id := filepath.Base(filepath.Clean(dir))
	if !pluginIDRe.MatchString(id) {
		return "", fmt.Errorf("id плагина %q не подходит: ожидается snake_case ^[a-z][a-z0-9_]*$", id)
	}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		entries, _ := os.ReadDir(dir)
		if len(entries) > 0 {
			return "", fmt.Errorf("папка %s не пуста — не перезаписываю", dir)
		}
	}
	// Файлы собираем ДО создания каталога: ошибка шаблона (плохая регулярка,
	// невставляемый параметр) не должна оставлять после себя папку с
	// половиной файлов.
	files, err := tpl.build(id, opts)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	out := map[string]string{
		"plugin.yaml":      files.manifest,
		"main.py":          files.mainPy,
		"plugin.test.yaml": files.tests,
		"README.md":        files.readme,
	}
	for name, content := range out {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return id, nil
}

func templateNames() string {
	names := make([]string, 0, 4)
	for _, t := range templateRegistry() {
		names = append(names, t.name)
	}
	return strings.Join(names, ", ")
}

func yamlQuote(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

func manifestTemplate(id string, opts CreateOptions) string {
	descLine := "description: " + yamlQuote(opts.Description)
	authorLine := "author: " + yamlQuote(opts.Author)
	if opts.Description == "" {
		descLine = `description: "TODO: что делает плагин, одной строкой"  # или сразу: plugin create <path> --description "..."`
	}
	if opts.Author == "" {
		authorLine = `author: "TODO"                # или сразу: plugin create <path> --author me`
	}
	header := `# Манифест плагина — контракт с ядром (подробнее: protocol/v0.2/PROTOCOL.md)
# Комментарии здесь — учебник для первого раза: когда освоитесь, смело чистите.
id: ` + id + `
version: 0.1.0              # семвер; маркетплейс ставит версии по тегу
platform_api: "^0.1"        # диапазон совместимости с ядром
` + descLine + `
` + authorLine + `

# ШПАРГАЛКА ВАЛИДАТОРА (освобождает от листания protocol/v0.2/PROTOCOL.md):
#   type портов:   string | number | boolean | array | object
#   format строк:  text | email | url | ip | file_ref     (file_ref: путь — cwd = папка плагина!)
#   type: array    — массив JSON-значений; элементы-объекты не типизируются

runtime:
  type: python              # python | binary | ...
  entry: main.py
  requires: []              # pip-зависимости (развернутся в изолированном окружении)

# Что плагин читает (ядро соберёт stdin-JSON ровно из этих полей).
# from — ДЕФОЛТНАЯ привязка к контексту; пайплайн всегда может перекрыть её
# через bind: в шаге (контракт v0.2). from можно опустить — тогда bind обязателен.
`
	if opts.Example == "array" {
		return header + `# Пример с МАССИВОМ (--example array) — обработчики результатов берут его:
input:
  items:
    from: input.items       # путь в контексте: input.* или steps.<шаг>.<поле>
    type: array             # в stdin придёт JSON-массив; проверяйте isinstance(list)!

# Что плагин обязан вернуть (ядро проверяет после КАЖДОГО запуска):
output:
  count:       { type: number }
  total_chars: { type: number }

# Декларативные разрешения (уровень L0): видны пользователю до установки
# и ревьюеру маркетплейса. Не заявленное здесь — подозрительно.
permissions:
  network: []               # [{ host: "api.example.com", port: 443 }]
  filesystem: none          # none | workspace
  secrets: []               # имена env-переменных с ключами API
`
	}
	return header + `input:
  text:
    from: input.text        # путь в контексте: input.* или steps.<шаг>.<поле>
    type: string
    format: text            # необяз.: text | email | url | ip | file_ref

# Что плагин обязан вернуть (ядро проверяет после КАЖДОГО запуска):
output:
  words: { type: number }
  chars: { type: number }

# Декларативные разрешения (уровень L0): видны пользователю до установки
# и ревьюеру маркетплейса. Не заявленное здесь — подозрительно.
permissions:
  network: []               # [{ host: "api.example.com", port: 443 }]
  filesystem: none          # none | workspace
  secrets: []               # имена env-переменных с ключами API
`
}

func mainPyTemplate(id string) string {
	tpl := `#!/usr/bin/env python3
"""{{ID}} — плагин оркестратора (сгенерировано: tool plugin create).

Протокол (protocol/v0.2/PROTOCOL.md v0.1):
  stdin  ← JSON со входом: ядро собирает его из полей input манифеста
  stdout → ровно один JSON-конверт:
           ok:    {"status": "ok", "output": {...}}               exit 0
           error: {"status": "error", "error": {...}}             exit 1 (доменная ошибка)
           исключение / мусор на stdout → платформенная ошибка →   exit 2
  stderr → свободные логи, ядро сложит их в журнал рана

Граница ответственности: плагин СООБЩАЕТ об ошибке; судьбу цепочки
(stop/skip/retry) решает конфиг пайплайна, а не плагин.
"""
import json
import sys


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return 1


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        # виновата вызывающая сторона — платформенная ошибка, стопит ран
        print(json.dumps({"status": "error", "error": {
            "code": "bad_input", "message": str(e), "retryable": False}},
            ensure_ascii=False))
        return 2

    text = str(data.get("text") or "").strip()
    if not text:
        # доменная ошибка: шаг отработал штатно, но результат отрицательный
        return fail("empty_input", "поле text пустое")

    # ── ваша логика здесь ──────────────────────────────────────
    words = len(text.split())
    chars = len(text)
    return ok({"words": words, "chars": chars})


if __name__ == "__main__":
    sys.exit(main())
`
	return strings.ReplaceAll(tpl, "{{ID}}", id)
}

func testsTemplate() string {
	return `# Контракт-тесты плагина. Прогон: tool plugin test <эта папка>
# Плагин гоняется через тот же subprocess-протокол, что и ядро в ране.
tests:
  - name: считает слова и символы
    input: { text: "hello world" }
    expect:
      status: ok
      output:
        words: 2                 # точное равенство
        chars: { equals: 11 }    # то же через матчер

  - name: пустой вход → доменная ошибка empty_input, не retryable
    input: { text: "  " }
    expect:
      status: error
      exit_code: 1
      error: { code: empty_input, retryable: false }

  - name: битый JSON → платформенная ошибка
    input_raw: "{broken"
    expect:
      exit_code: 2
      error: { code: platform:bad_input, retryable: false }

# Полезные матчеры: { present: true } { absent: true } { contains: "..." } { type: "array" }
# env: { KEY: "value" } — переменные на время теста (секреты, mock-режимы)
# input_raw: "{oops"   — сырой stdin, для тестов битого JSON (ожидайте exit 2)
#
# Плагину нужен живой HTTP-сервер (донор ходит по сети)? Добавьте сервис —
# он поднимается на 127.0.0.1 до кейсов и гасится после прогона:
# services:
#   - type: http
#     root: fixtures/site        # каталог, который отдаётся (от этой папки)
#     base_url: "{{base_url}}"   # подставляется в input / input_raw / env
# тогда целью кейса пишите url: "{{base_url}}/index.html"
`
}

// mainPyArrayTemplate — вариант --example array (M5, тестер №3): шаблон со
// string-входом не учил работе с массивами, и типизация input казалась
// «менее очевидной, чем хотелось бы». Здесь — та же логика обучения, но для array.
func mainPyArrayTemplate(id string) string {
	tpl := `#!/usr/bin/env python3
"""{{ID}} — TODO: что делает плагин, одной строкой.

Шаблон с входом-МАССИВОМ (--example array).

Протокол (protocol/v0.2/PROTOCOL.md):
  stdin  ← JSON {"items": [...]} — ядро собирает его из полей input манифеста
  stdout → ровно один JSON-конверт:
           ok:    {"status": "ok", "output": {...}}               exit 0
           error: {"status": "error", "error": {...}}             exit 1 (доменная ошибка)
           исключение / мусор на stdout → платформенная ошибка →   exit 2
  stderr → свободные логи, ядро сложит их в журнал рана

ВАЖНО про типы: валидатор проверяет совместимость портов СТАТИЧЕСКИ
(tool validate), но в рантайме тип входа плагину не гарантирован —
массив проверяйте сами (см. guard ниже). Это и есть контракт.
"""
import json
import sys


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return 1


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        # виновата вызывающая сторона — платформенная ошибка, стопит ран
        print(json.dumps({"status": "error", "error": {
            "code": "bad_input", "message": str(e), "retryable": False}},
            ensure_ascii=False))
        return 2

    items = data.get("items")
    # guard: type: array объявлен в манифесте, но runtime-гарантии нет —
    # защита честного плагина (и пример для ваших контракт-тестов)
    if not isinstance(items, list):
        print(json.dumps({"status": "error", "error": {
            "code": "bad_input",
            "message": "items должен быть массивом (type: array в манифесте)",
            "retryable": False}}, ensure_ascii=False))
        return 2

    # ── ваша логика здесь ──────────────────────────────────────
    # пример: привести элементы к строкам и посчитать суммарную длину
    texts = [str(x) for x in items]
    return ok({"count": len(items), "total_chars": sum(len(t) for t in texts)})


if __name__ == "__main__":
    sys.exit(main())
`
	return strings.ReplaceAll(tpl, "{{ID}}", id)
}

func testsArrayTemplate() string {
	return `# Контракт-тесты плагина (вариант --example array). Прогон: tool plugin test <эта папка>
# Плагин гоняется через тот же subprocess-протокол, что и ядро в ране.
tests:
  - name: считает элементы массива и суммарную длину
    input: { items: ["ab", "cde"] }
    expect:
      status: ok
      output:
        count: 2                # точное равенство
        total_chars: 5

  - name: пустой массив — корректный вход, а не ошибка
    input: { items: [] }
    expect:
      status: ok
      output: { count: 0, total_chars: 0 }

  - name: не-массив → платформенная ошибка (guard типа)
    input: { items: "oops" }
    expect:
      exit_code: 2            # виновата вызывающая сторона, не домен

# Полезные матчеры: { present: true } { absent: true } { contains: "..." } { type: "array" }
# env: { KEY: "value" } — переменные на время теста (секреты, mock-режимы)
# input_raw: "{oops"   — сырой stdin, для тестов битого JSON (ожидайте exit 2)
#
# Плагину нужен живой HTTP-сервер (донор ходит по сети)? Добавьте сервис —
# он поднимается на 127.0.0.1 до кейсов и гасится после прогона:
# services:
#   - type: http
#     root: fixtures/site        # каталог, который отдаётся (от этой папки)
#     base_url: "{{base_url}}"   # подставляется в input / input_raw / env
# тогда целью кейса пишите url: "{{base_url}}/index.html"
`
}

func pluginReadmeTemplateWithOpts(id string, opts CreateOptions) string {
	desc := opts.Description
	if desc == "" {
		desc = "TODO: одна строка — что делает плагин, что читает, что пишет."
	}
	// author not needed in README but we keep desc
	return `# ` + id + `

` + desc + `

## Цикл разработки

` + "```bash" + `
# правим main.py, затем:
tool plugin test ` + id + `      # контракт-тесты (зелёные из коробки)
tool plugin validate ` + id + `  # проверка манифеста
` + "```" + `

Протокол и правила контракта: protocol/v0.2/PROTOCOL.md в корне репозитория.
`
}
