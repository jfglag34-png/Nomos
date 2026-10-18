// Файл genshell.go — всё, что пишется в каталог плагина, кроме манифеста:
// адаптер main.py, контракт-тесты и README.
//
// Три решения, которые здесь приняты и которые выглядят как недоработка:
//
//  1. main.py НЕ вызывает функцию донора. Импортёр умеет поставить модуль на
//     sys.path и загрузить из него функцию — это механика, которую можно
//     проверить. Сигнатуру функции мы не проверяли: прочитать её, не
//     выполняя чужой код, нельзя, а угадать — значит отдать плагин, который
//     падает с ошибкой без внятной причины. Поэтому все пути ведут к
//     доменной ошибке с объяснением, а не к ok.
//
//  2. Контракт-тесты ровно три, и все три зелёные на СКЕЛЕТЕ, а не на
//     доноре: битый JSON, пустой вход и честный отказ на корректном входе.
//     Тест «мы позвали донора и получили его ответ» был бы враньём, пока
//     вызова нет. Три — потому что столько требует проверка реестра
//     (internal/cli/registry.go), а не потому что три красиво.
//
//  3. README — не визитка, а список того, что осталось за человеком. Если
//     его не читать, плагин выглядит готовым.

package importer

import (
	"fmt"
	"strings"
)

// readmeFence — ограда блока кода в README. В Go нельзя держать обратные
// кавычки внутри raw string, поэтому ограда собирается конкатенацией —
// ровно так, как это сделано в internal/core/plugincreate.go.
const readmeFence = "```"

// renderFiles собирает содержимое всех файлов до записи. Порядок — от
// главного к приложениям, и он фиксирован: два запуска на одном плане дают
// одинаковый Result.Files.
func renderFiles(skel skeleton, res Result) []generatedFile {
	files := []generatedFile{
		{name: "plugin.yaml", content: renderManifest(skel)},
		{name: "main.py", content: renderMainPy(skel)},
		{name: "plugin.test.yaml", content: renderTests()},
		{name: "README.md", content: renderReadme(skel, res)},
	}
	// Lock — только когда есть что записывать (см. decideRequirements): пустой
	// requirements.lock выглядел бы как «зависимости проверены», а это неправда.
	if len(skel.specs) > 0 {
		files = append(files, generatedFile{name: "requirements.lock", content: renderRequirementsLock(skel.specs)})
	}
	return files
}

// mainPyTemplate — адаптер донора. Структура (ok / fail / main и три формы
// ответа) повторяет шаблон internal/core/plugincreate.go: два генератора
// обязаны выглядеть родными, иначе человек, читавший один, не узнает другой.
const mainPyTemplate = `#!/usr/bin/env python3
"""{{ID}} — адаптер плагина WEDRA для чужого Python-проекта.

СГЕНЕРИРОВАНО ИМПОРТЁРОМ. Это СКЕЛЕТ, а не работающий плагин: он умеет
поставить модуль донора на sys.path и загрузить из него функцию, но НЕ умеет
её вызвать. Сигнатуру чужой функции никто не проверял, а выдумывать её —
значит отдать плагин, который упадёт с ошибкой, не подсказывающей причину.
Место, где должен появиться вызов, помечено «TODO(человек)».

Все пути здесь ведут к внятной ошибке, а не к молчаливому успеху:
  * битый или не-объект JSON на stdin → платформенная ошибка bad_input, exit 2;
  * вход без порта request → доменная ошибка input_missing, exit 1;
  * корректный вход → донор грузится и тут же честно отказывается работать,
    код donor_not_wired либо donor_unavailable, exit 1.

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
import importlib
import json
import os
import sys


# ── что записал импортёр ────────────────────────────────────────────────────
# DONOR_ROOT — каталог проекта-донора, который добавляется в sys.path.
# Он переопределяется переменной окружения WEDRA_DONOR_ROOT: путь к каталогу
# у каждого своя, а хранить его в манифесте нельзя (схема не имеет такого
# поля), и зашитый абсолютный путь с чужой машины — тоже враньё, поэтому в
# этом файле он честно помечен как «записан при генерации».
DONOR_ROOT = {{DONOR_ROOT}}
# Точка входа донора. Пустые строки означают, что выбрать было не на чем:
# импортёр не станет угадывать функцию по имени.
DONOR_MODULE = {{DONOR_MODULE}}
DONOR_ATTR = {{DONOR_ATTR}}
DONOR_ENTRY = {{DONOR_ENTRY}}


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return 1


def platform_fail(code, message):
    """Ошибка вины вызывающей стороны — ядро обязано стопит ран.

    Отдельная функция, а не флаг у fail(), потому что код возврата здесь
    другой (2 вместо 1), перепутать их легко, а в журнале рана это читается
    как «упал плагин», и виноватым окажется не тот.
    """
    print(json.dumps({"status": "error", "error": {
        "code": code, "message": message, "retryable": False}},
        ensure_ascii=False))
    return 2


def donor_root():
    """Каталог донора или причина, почему его нет.

    Относительный путь не разрешается: плагин запускается из своей папки, и
    «src» значил бы совсем другое. Тихий переход в cwd дал бы обрывок чужого
    пакета вместо понятного отказа.
    """
    root = os.environ.get("WEDRA_DONOR_ROOT") or DONOR_ROOT
    if not root:
        return None, ("каталог донора не задан: впишите DONOR_ROOT в этот файл "
                      "или задайте WEDRA_DONOR_ROOT при запуске")
    if not os.path.isabs(root):
        return None, ("каталог донора %s записан относительным, а плагин запускается из своей "
                      "папки: укажите абсолютный путь в DONOR_ROOT или через WEDRA_DONOR_ROOT" % root)
    return root, None


def load_donor():
    """Загрузить модуль донора и функцию внутри него.

    Возвращает (функция, None) либо (None, (код, сообщение)). Вызова здесь
    нет и не будет — это решение импортёра, а не недосмотр.
    """
    if not DONOR_MODULE or not DONOR_ATTR:
        return None, ("donor_not_wired",
                      "точка входа донора не выбрана — функцию надо выбрать вручную в редакторе плагинов")

    root, problem = donor_root()
    if problem:
        return None, ("donor_unavailable", problem)
    if root not in sys.path:
        sys.path.insert(0, root)

    try:
        module = importlib.import_module(DONOR_MODULE)
    except Exception as exc:
        # Перехватываем всё, а не только ImportError: чужой пакет на этапе
        # импорта поднимает что угодно (конфиг не найден, битый .pth, нет
        # бинарной библиотеки). Текст ошибки уходит в message — стектрейс
        # наружу не отдаём, он объясняет строку внутри донора, а не то, что
        # делать человеку.
        return None, ("donor_unavailable",
                      "не импортируется %s из %s: %s: %s" % (DONOR_MODULE, root, type(exc).__name__, exc))

    missing = object()
    target = module
    for part in [p for p in DONOR_ATTR.split(".") if p]:
        target = getattr(target, part, missing)
        if target is missing:
            return None, ("donor_not_wired",
                          "в модуле %s нет %s — точку входа выбрать не на чем, функцию надо выбрать "
                          "вручную в редакторе плагинов" % (DONOR_MODULE, DONOR_ATTR))
    if not callable(target):
        return None, ("donor_not_wired",
                      "%s:%s найдено, но это не функция: выберите другую точку входа вручную "
                      "в редакторе плагинов" % (DONOR_MODULE, DONOR_ATTR))
    return target, None


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        # виновата вызывающая сторона — платформенная ошибка, стопит ран
        return platform_fail("bad_input", str(e))
    if not isinstance(data, dict):
        return platform_fail("bad_input", "на stdin ожидался JSON-объект, а пришёл %s" % type(data).__name__)

    request = data.get("request")
    if request is None or request == "":
        # доменная ошибка: шаг отработал штатно, но результата нет. Так же,
        # как пустой text в скелете из команды plugin create.
        return fail("input_missing",
                    "порт input.request пуст: плагин не знает, что передать функции донора")

    donor_fn, problem = load_donor()
    if problem:
        code, message = problem
        return fail(code, message)

    # ── TODO(человек): здесь должен быть вызов донора ────────────────────
    # Функция загружена (DONOR_ENTRY — как она выглядит в чужом проекте), но
    # НЕ вызвана. Напишите вызов, разберите ответ и верните ok({...}):
    # поля сверяются с манифестом, лишние ядро отбросит, недостающие — признает
    # нарушением контракта.
    #
    # Один вызов и один разбор — ровно то, что импортёр отказался угадывать.
    # Пока его нет, плагин отказывается работать и говорит об этом прямо.
    return fail("donor_not_wired",
                "функция %s загружена, но не вызвана: сигнатуру импортёр не проверял. "
                "Вызов напишите вручную в редакторе плагинов" % (DONOR_ENTRY or DONOR_MODULE))


if __name__ == "__main__":
    sys.exit(main())
`

// renderMainPy подставляет в шаблон константы донора. Все значения
// экранируются как строковые литералы Python: путь донора на Windows
// содержит обратные слэши, а «C:\Users» без экранирования — это escape-последовательность
// \U, и модуль не импортируется вовсе.
func renderMainPy(skel skeleton) string {
	replacer := strings.NewReplacer(
		"{{ID}}", skel.id,
		"{{DONOR_ROOT}}", pyString(skel.donorRoot),
		"{{DONOR_MODULE}}", pyString(skel.donorModule),
		"{{DONOR_ATTR}}", pyString(skel.donorAttr),
		"{{DONOR_ENTRY}}", pyString(skel.donorEntry),
	)
	return replacer.Replace(mainPyTemplate)
}

// renderTests — три кейса, зелёные на скелете при любом доноре. Ни один из них
// не проверяет работу донора: вызова нет, и тест, который бы его проверял,
// был бы написанием несуществующего ожидания.
func renderTests() string {
	return `# Контракт-тесты сгенерированного скелета. Прогон: wedra plugin test <эта папка>
#
# Здесь ровно три кейса, и все три проверяют СКЕЛЕТ, а не донора: вызова
# функции донора в адаптере ещё нет, и тест, ждущий её ответа, был бы
# написанием несуществующего ожидания. Когда вызов появится — добавьте кейсы
# на реальные данные и переименуйте этот комментарий.
tests:
  - name: битый JSON на stdin → платформенная ошибка bad_input
    input_raw: "{broken"
    expect:
      exit_code: 2
      error: { code: platform:bad_input, retryable: false }

  - name: вход без порта request → доменная ошибка input_missing
    input: {}
    expect:
      status: error
      exit_code: 1
      error: { code: input_missing, retryable: false }

  - name: корректный вход → скелет отказывается работать и говорит почему
    input:
      request: { probe: true }
    expect:
      status: error
      exit_code: 1
      # код не фиксируем: donor_not_wired, если донор не выбран, и
      # donor_unavailable, если выбран, но не импортируется. Оба честны,
      # и различать их должен текст сообщения, а не этот тест.

# Полезные матчеры: { present: true } { absent: true } { contains: "..." } { type: "array" }
# env: { KEY: "value" } — переменные на время теста (WEDRA_DONOR_ROOT здесь и
# пригодится: указать плагину каталог донора, не переписывая файл)
# input_raw: "{oops"   — сырой stdin, для тестов битого JSON (ожидайте exit 2)
`
}

// renderReadme — список того, что осталось за человеком. Пишется после
// decide(), поэтому читает те же блокеры, что и Result, и расходиться с ним
// не может.
func renderReadme(skel skeleton, res Result) string {
	var b strings.Builder
	b.WriteString("# " + skel.id + "\n\n")
	b.WriteString(skel.description + "\n\n")

	b.WriteString("## Что это\n\n")
	b.WriteString("Каталог создан импортёром чужого Python-проекта. Манифест валиден, ")
	b.WriteString("контракт-тесты проходят — и это всё, что можно было сказать без вранья.\n\n")
	if skel.descFromDonor {
		b.WriteString("Описание выше взято из метаданных донора и описывает ДОНОРА, а не этот плагин. ")
		b.WriteString("Замените его своим текстом, когда допишете вызов.\n\n")
	}
	if skel.donorReadme != "" {
		b.WriteString("Что вообще делает донор — в его README: " + skel.donorReadme + "\n\n")
	}

	b.WriteString("| файл | что это |\n|---|---|\n")
	b.WriteString("| plugin.yaml | контракт с ядром. Порты — заглушки, имена не проверены |\n")
	b.WriteString("| main.py | адаптер: ставит донору sys.path, грузит функцию, **не вызывает** |\n")
	b.WriteString("| plugin.test.yaml | три кейса на сам скелет |\n")
	b.WriteString("| README.md | этот файл |\n")
	if len(skel.specs) > 0 {
		b.WriteString("| requirements.lock | точные версии, обязаны совпадать с runtime.requires построчно |\n")
	}
	b.WriteString("\n")

	b.WriteString("## Чего плагин НЕ умеет\n\n")
	b.WriteString("**Вызова функции донора нет.** main.py доводит дело до загрузки функции и ")
	b.WriteString("останавливается. Это не недосмотр генератора: сигнатуру чужой функции можно ")
	b.WriteString("узнать только чтением её кода или вызовом, а импортёр не исполняет чужой код ")
	b.WriteString("и не выдумывает API. Пока вызова нет, плагин на любом входе возвращает ")
	b.WriteString("доменную ошибку и НИКОГДА не возвращает ok.\n\n")
	b.WriteString("Что заменяет работу: ошибки `donor_not_wired` (точка входа не выбрана) и ")
	b.WriteString("`donor_unavailable` (каталог донора не задан или модуль не импортируется). ")
	b.WriteString("Обе с explain в message — по ним видно, что чинить.\n\n")

	b.WriteString("## Что осталось решить человеку\n\n")
	if len(res.Blockers) == 0 {
		b.WriteString("Блокеров нет — но вызов функции донора всё равно не написан, см. раздел выше.\n")
	} else {
		b.WriteString("Это не список пожеланий: пока пункт не закрыт, плагин не работает.\n\n")
		for i, blocker := range res.Blockers {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, blocker))
		}
	}
	b.WriteString("\n")

	b.WriteString("## Как подключить донора\n\n")
	if skel.donorRoot != "" {
		b.WriteString("В main.py записан `DONOR_ROOT` = " + skel.donorRoot + " — путь, который был ")
		b.WriteString("у донора в момент генерации. Если проект лежит в другом месте, задайте ")
		b.WriteString("переменную окружения WEDRA_DONOR_ROOT или поправьте константу вручную; ")
		b.WriteString("относительный путь плагин не принимает.\n\n")
	} else {
		b.WriteString("Каталог донора неизвестен: впишите `DONOR_ROOT` в main.py (абсолютный путь ")
		b.WriteString("к каталогу, где лежит пакет) или задайте WEDRA_DONOR_ROOT при запуске.\n\n")
	}
	if skel.donorEntry != "" {
		b.WriteString("Точка входа донора: " + skel.donorEntry + ". ")
	}
	b.WriteString("Точка входа в манифесте не записывается — такого поля в схеме нет ")
	b.WriteString("(schemas/manifest.schema.json), и ядро всё равно ищет плагин по entry.\n\n")

	b.WriteString("## Цикл разработки\n\n")
	b.WriteString(readmeFence + "bash\n")
	b.WriteString("# 1. написать вызов донора в main.py вместо отказа\n")
	b.WriteString("# 2. переименовать порты в plugin.yaml под реальные данные\n")
	b.WriteString("# 3. и только потом — прогон:\n")
	b.WriteString("wedra plugin test " + skel.id + "       # контракт-тесты\n")
	b.WriteString("wedra plugin validate " + skel.id + "   # проверка манифеста\n")
	b.WriteString(readmeFence + "\n\n")
	b.WriteString("Протокол и правила контракта: protocol/v0.2/PROTOCOL.md в корне репозитория.\n")
	return b.String()
}

// pyString — строковый литерал Python. Кавычки и обратные слэши экранируются:
// путь донора на Windows вида C:\Users\… без экранирования превращается в
// escape-последовательность \U, и модуль не импортируется никогда.
func pyString(value string) string {
	return `"` + escapeDoubleQuoted(value) + `"`
}

// escapeDoubleQuoted экранирует содержимое для двойных кавычек. Набор
// escape-последоваций у YAML и Python здесь совпадает (\\, \", \n, \r, \t,
// \xNN), поэтому экранировщик один на оба формата — но функции-обёртки разные:
// правила у форматов разные, и путать их не надо.
func escapeDoubleQuoted(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
