#!/usr/bin/env python3
"""oletools — анализ макросов офисного документа (обёртка над olevba).

Вход (stdin JSON): file (путь к документу, обязателен), wall_timeout
(опц., 300).

Вызов: <OLETOOLS_BIN|python3 -m oletools.olevba> -j -a <абс.путь>
       (cwd = временная папка).
`-j/--json` — машинный режим olevba (oletools 0.60+), он не дефолтный; `-a`
гасит печать исходников макросов (display_code=False), поэтому в вывод
плагина код не попадает.

Что донор реально отдаёт в stdout при -j (проверено на olevba 0.60.2 и на
трёх фикстурах из fixtures/, собранных скриптом make_ole.py):

    macro.doc      OLE с VBA-проектом: type "OLE", macros[0] =
                   {vba_filename "Module1.bas", subfilename "macro.doc",
                   ole_stream "VBA/Module1", code null}, analysis = четыре
                   находки — AutoExec/AutoOpen и Suspicious на Shell, vbHide,
                   powershell;
    nomacros.doc   OLE без макросов: type "OLE", macros [], analysis null;
    truncated.doc  каталог обещает секторы за концом файла: olevba падает,
                   exit 8, а в массив попадает объект {"type": "msg"}.

Формат stdout: ОДИН JSON-массив `[...]`. Его открывает
`log_helper.enable_logging()` (печатает `[` в stdout), каждый объект печатает
`print_json()` — первый с отступом, остальные с `,` + отступ перед ним, что
ровно разделитель элементов массива, — а закрывает `log_helper.end_logging()`
(печатает `]`). Перед массивом донор может напечатать предупреждение про
кодировку stdout («Encoding for stdout is only cp1251…») — это не JSON, и
разбор не должен на нём спотыкаться.

Выход (stdout JSON): {file, file_type, macros[], findings[], count,
tool_version}. file — строка оператора: абсолютная форма ничего бы не
добавила, а привязала бы вывод и журнал рана к машине.

Имена полей и значения взяты из ответа донора, а не из словаря обёртки:
file_type — это его же `type` (OLE, OpenXML, Text, XML, MHTML, RTF, SLK),
type у находки — AutoExec / Suspicious / IOC, keyword и description — строки
его таблиц ключевых слов. Пустая строка означала бы «эта версия оlevba не
знает такой тип», и «не знаю» с «нечем» не путались бы.

Чего в выводе НЕТ и почему:

  * `code` макроса — это исходник. Плагин разбирает документ, а не публикует
    его содержимое: код уходит в контекст рана, журнал и дальше в отчёты, а
    там он неотличим от данных. `-a` у донора и не даёт его напечатать, но
    поле всё равно не копируется — по списку ДОПУЩЕННЫХ имён.
  * Абсолютный путь в subfilename — см. выше про cwd: донор пишет в это поле
    ровно то, что получил в argv, поэтому argv содержит только имя файла.
  * `container` — путь контейнера, в котором лежит вложенный OLE-файл
    (например `word/embeddings/oleObject1.bin`). Это внутренний идентификатор
    структуры документа; наружу не отдаётся.
  * `python_version` из MetaInformation — свойство МАШИНЫ, а не документа.
  * `url` из MetaInformation — рекламная строка олеTools.
  * `do_deobfuscate`, `show_pcode`, `code_deobfuscated` — это настройки
    запуска и результат деобфускации, а не находки. Деобфускация не
    включается: она меняет текст макроса, и в выводе оказался бы уже не тот
    код, который в файле.

Из MetaInformation берётся РОВНО одно поле — version. Без него «макросов не
нашли» неотличимо от «эта версия olevba не знает такой формат», поэтому
пустой строкой это не подменяется, а отсутствие версии — ошибка разбора.

Доменные ошибки: empty_file, missing_file, oletools_not_installed, timeout
(retryable), no_report, tool_failed. Платформенные (exit 2): битый JSON входа,
wall_timeout не число/неположителен, нечитаемый репорт, отсутствие version в
MetaInformation.
"""
import json
import os
import shutil
import subprocess
import sys

DEFAULT_WALL = 300

# stderr донора — диагностика, а не результат; предел нужен, чтобы донос не
# вылил в stderr плагина свой простырь (ядро кладёт на него 1 МБ).
STDERR_LIMIT = 8000

# Поля репорта, которые разрешено копировать наружу. Список, а не запрет:
# новое поле донора не должно само собой утекать в вывод плагина.
REPORT_FIELDS = ("type", "json_conversion_successful", "macros", "analysis")

# Поля макроса, которые разрешено копировать (code — не в списке, см. выше).
MACRO_FIELDS = ("vba_filename", "subfilename", "ole_stream")

# Поля находки, которые разрешено копировать. Все три — строки словарей
# донора; своими словами не пересказываем.
FINDING_FIELDS = ("type", "keyword", "description")


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


def read_json_objects(text):
    """Разбирает stdout olevba в режиме -j в список объектов.

    Настоящий olevba печатает ОДИН JSON-массив (см. модульную документацию),
    поэтому сначала пробуем целиком json.loads(). Если не вышло — читаем
    объекты потоком через raw_decode, пропуская разделители `,` и скобки
    массива.

    Отдельно про мусор ВНЕ массива: olevba печатает «Encoding for stdout is
    only cp1251, will auto-encode text with utf8 before output» перед '['.
    На проверенной машине эта строка уходит в stderr (проверено: с 2>/dev/null
    stdout остаётся чистым JSON), но её поток зависит от кодировки консоли, а
    не от контракта донора. Поэтому разбор ищет первую '[' или '{' и начинает
    с неё: строка, которая сегодня в stderr, завтра окажется в stdout — и
    плагин не превратится из «разобрал документ» в «репорт не разбирается».
    """
    text = text or ""
    try:
        parsed = json.loads(text)
    except ValueError:
        pass
    else:
        return parsed if isinstance(parsed, list) else [parsed]

    decoder = json.JSONDecoder()
    objects = []
    length = len(text)
    start = min([i for i in (text.find("["), text.find("{")) if i != -1]
                or [length])
    pos = start
    while pos < length:
        while pos < length and (text[pos].isspace() or text[pos] in ",[]"):
            pos += 1
        if pos >= length:
            break
        obj, pos = decoder.raw_decode(text, pos)
        objects.append(obj)
    return objects


def resolve_bin():
    """Команда запуска донора.

    По умолчанию — `python3 -m oletools.olevba`, а не console-script `olevba`:
    pip кладёт скрипты в каталог Scripts (на Windows), а он часто не в PATH, и
    тогда донор установлен и при этом «не найден». Модуль доступен ровно
    тогда, когда установлен пакет, то есть проверка «установлен ли донор» не
    врёт.

    OLETOOLS_BIN — штатный переключатель (путь, имя из PATH или .py-мок);
    значение приходит из окружения хоста, поэтому в argv попадает как есть и
    дополнительно не цитируется: список аргументов не даёт инъекции.
    """
    bin_env = os.environ.get("OLETOOLS_BIN", "").strip()
    if not bin_env:
        return [sys.executable, "-m", "oletools.olevba"]
    if "/" not in bin_env and "\\" not in bin_env:
        bin_env = shutil.which(bin_env) or os.path.abspath(bin_env)
    else:
        bin_env = os.path.abspath(bin_env)
    if bin_env.lower().endswith(".py"):
        # .py-мок запускаем через интерпретатор: прямой exec непереносим
        # (shebang+CRLF на Linux, ассоциации на Windows).
        return [sys.executable, bin_env]
    return [bin_env]


def parse_wall(data):
    """(секунды|None, текст ошибки|None).

    Отсутствие ключа и явный 0 — разные вещи: наивный
    `data.get("wall_timeout") or DEFAULT_WALL` превращал 0 в 300, то есть ран
    просил «без времени», а плагин отработал полный лимит и отчитался ok.
    """
    raw = data.get("wall_timeout")
    if raw is None:
        return float(DEFAULT_WALL), None
    if isinstance(raw, bool):
        return None, "wall_timeout обязан быть числом, а не true/false"
    try:
        wall = float(raw)
    except (TypeError, ValueError):
        return None, "wall_timeout обязан быть числом"
    if wall <= 0:
        return None, "wall_timeout обязан быть положительным числом"
    return wall, None


def main():
    try:
        sys.stdin.reconfigure(encoding="utf-8", errors="replace")
        sys.stdout.reconfigure(encoding="utf-8")
        sys.stderr.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass

    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail("bad_input", "невалидный JSON на входе: %s" % e, exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    target = str(data.get("file") or "").strip()
    if not target:
        return fail("empty_file", "file пуст")

    wall, wall_err = parse_wall(data)
    if wall_err:
        return fail("bad_wall_timeout", wall_err, exit_code=2)

    file_path = os.path.abspath(os.path.expanduser(target))
    # Путь проверяет обёртка, а не донор: иначе «нет такого файла» и «файл
    # нечитаем» были бы одним и тем же словом с кодом от олеTools.
    if not os.path.isfile(file_path):
        return fail("missing_file", "файл не найден: %s" % target)

    # cwd = каталог документа, а во временной папке, и аргумент — ТОЛЬКО имя
    # файла. Причина в том, что копируется наружу: olevba пишет в subfilename
    # ровно то, что получил в argv, то есть с абсолютным путём в него попал бы
    # путь машины (а на Windows он ещё и с обратными слэшами и кириллицей —
    # значение, которое нельзя ни сравнить в тесте, ни перенести на другую
    # машину). Каталог документа вместо временной папки выбран потому, что
    # относительный путь иначе не построить: через os.path.relpath получилось
    # бы «../../../..» (и ошибку на разных дисках), а ребёнок cwd всё равно
    # жёстче не нужен — olevba только читает.
    cmd = resolve_bin() + ["-j", "-a", os.path.basename(file_path)]
    run_dir = os.path.dirname(file_path) or os.curdir

    try:
        proc = subprocess.run(cmd, capture_output=True, text=True,
                              encoding="utf-8", errors="replace",
                              cwd=run_dir, timeout=wall)
    except FileNotFoundError:
        return fail("oletools_not_installed",
                    "olevba не найден: pip install oletools==0.60.2 "
                    "(или укажите OLETOOLS_BIN)")
    except subprocess.TimeoutExpired:
        return fail("timeout",
                    "olevba не уложился в %.0fs: уменьшите размер "
                    "документа или увеличьте wall_timeout" % wall,
                    retryable=True)
    if proc.stderr:
        sys.stderr.write(proc.stderr[:STDERR_LIMIT])

    # Ненулевой код возврата — отказ, даже если stdout успел напечатать что-то
    # похожее на репорт: на битом файле olevba печатает в массив объект
    # {"type": "msg"} с текстом ошибки и уходит с кодом 8. Такой «репорт»
    # нельзя выдавать за «проверили и нашли вот столько».
    if proc.returncode != 0:
        tail = (proc.stderr or "").strip().splitlines()
        last = tail[-1] if tail else "exit %d" % proc.returncode
        return fail("tool_failed",
                    "olevba упал (exit %d): %s" % (proc.returncode, last))

    try:
        objects = read_json_objects(proc.stdout or "")
    except ValueError as e:
        return fail("bad_report",
                    "JSON-репорт olevba не разбирается: %s" % e, exit_code=2)

    meta = None
    report = None
    for obj in objects:
        if not isinstance(obj, dict):
            continue
        if obj.get("type") == "MetaInformation" or "script_name" in obj:
            meta = obj
            continue
        report = obj
        break

    if report is None:
        return fail("no_report",
                    "olevba не дал JSON-репорт по файлу (нет репорта или он "
                    "пуст)")
    if not report.get("json_conversion_successful"):
        return fail("bad_report",
                    "olevba не смог разобрать документ "
                    "(json_conversion_successful != true)", exit_code=2)
    raw_macros = report.get("macros")
    if not isinstance(raw_macros, list):
        return fail("bad_report", "в репорте olevba нет списка macros",
                    exit_code=2)
    raw_analysis = report.get("analysis")
    # null analysis — это «макросов нет», документ разобран (так отдаёт
    # донор на фикстуре nomacros.doc), и это НЕ ошибка. А вотanalysis, который
    # не список и не null, — это другая форма ответа: молчать и выдать ноль
    # находок значило бы выдать «проверили и не нашли» вместо «не поняли».
    if raw_analysis is not None and not isinstance(raw_analysis, list):
        return fail("bad_report",
                    "analysis в репорте olevba — не список и не null",
                    exit_code=2)

    # Копируем по списку разрешённых имён: code, container и всё новое, что
    # донор принесёт, наружу не идёт.
    picked = {field: report.get(field) for field in REPORT_FIELDS}
    macros = []
    for macro in picked.get("macros") or []:
        if not isinstance(macro, dict):
            continue
        macros.append({field: str(macro.get(field) or "")
                       for field in MACRO_FIELDS})

    findings = []
    for item in picked.get("analysis") or []:
        if not isinstance(item, dict):
            continue
        findings.append({field: str(item.get(field) or "")
                         for field in FINDING_FIELDS})

    # Версия донора обязательна: без неё «макросов нет» неотличимо от «эта
    # версия olevba не знает такой формат».
    version = str((meta or {}).get("version") or "").strip()
    if not version:
        return fail("bad_report",
                    "в MetaInformation olevba нет строкового поля version "
                    "(разбор приписан неизвестной версии)", exit_code=2)

    return ok({"file": target,
               "file_type": str(picked.get("type") or ""),
               "macros": macros,
               "findings": findings,
               "count": len(findings),
               "tool_version": version})


if __name__ == "__main__":
    sys.exit(main())
