#!/usr/bin/env python3
"""pefile — разбор PE-файла (донор: pefile, библиотека без CLI, паттерн C).

Вход (stdin JSON): file (путь к PE-файлу, обязателен), wall_timeout (опц., 300).

Вызов: <PEFILE_BIN|python3 -c SNIPPET> <абс.путь к файлу>, cwd — пустой
временный каталог. SNIPPET — константа модуля: он импортирует pefile, читает
COFF-заголовок, таблицу секций и каталог импортов и печатает один JSON в
stdout. main.py саму pefile не импортирует (только stdlib) и в сеть не ходит.

Что донор реально отдаёт (проверено на pefile 2024.8.26, фикстура
fixtures/sample.dll — 18 секций, 6 импортируемых DLL, 28 символов):

    {"parser_version": "2024.8.26",
     "machine_id": 34404, "machine": "IMAGE_FILE_MACHINE_AMD64",
     "sections": [{"name": ".text", "virtual_address": 4096, "size": 7168,
                   "characteristics": 1610612768, "flags": [...]}],
     "imports": [{"dll": "KERNEL32.dll",
                  "symbols": [{"name": "GetTickCount", "ordinal": null}]}]}

Выбор полей и обоснование:

  parser_version → наружу. Версия парсера — часть ответа: без неё «импортов
      нет» неотличимо от «эта версия pefile не разобрала каталог импортов».
      Пустой строкой это не подменить, поэтому репорт без неё считается битым.
  machine_id     → FILE_HEADER.Machine как есть, число. Это то, что лежит в
      файле; «красивое имя» без него не восстановить.
  machine        → имя ИЗ СЛОВАРЯ ДОНОРА (pefile.MACHINE_TYPE), не из таблицы
      обёртки. Измерено на pefile 2024.8.26: в словаре 35 числовых кодов,
      прежняя таблица обёртки знала 10, то есть 25 кодов (включая 0x0
      IMAGE_FILE_MACHINE_UNKNOWN) она назвала бы «0x0000».
      Пустая строка означает «эта версия донора не знает такой код», и
      machine_id при этом заполнен: «не знаю» и «нечем» не путаются.
  sections[].name → имя секции, обрезанное по NUL. Имена вида "/4", "/14" —
      это НЕ имена: в COFF длинное имя лежит в строковой таблице, а в самой
      таблице секций стоит ссылка "/<смещение>". Обёртка отдаёт ссылку как
      есть и не разыменовывает её: разыменование было бы нашим решением, а не
      выводом донора.
  sections[].size → SizeOfRawData, то есть размер НА ДИСКЕ, а не
      VirtualSize: у .bss в фикстуре он 0 при 512 байтах в памяти.
  sections[].characteristics → число-битмаска, как в файле.
  sections[].flags  → те же биты, разложенные в имена ИЗ СЛОВАРЯ ДОНОРА
      (pefile.SECTION_CHARACTERISTICS). Измерено: 31 однобитовая запись и
      ещё 11 многобитовых масок выравнивания (IMAGE_SCN_ALIGN_*).
      Раскладываются только однобитовые: по многобитовым «маска & запись»
      дала бы несколько имён сразу, а выбрать «правильное» — значило бы
      придумать правило, которого в доноре нет. Само число characteristics
      отдаётся всегда, поэтому ничего не теряется.
  imports[].symbols[].name / .ordinal — имя и/или номер импорта. Импортировать
      можно по номеру, и тогда имени нет: раньше обёртка превращала это в
      строку "#17", то есть выдумывала тип, которого в файле нет.
  parser_version см. выше.

Чего в выводе НЕТ и почему:

  FileHeader.TimeDateStamp — время сборки. Не результат, и от него вывод
                  меняется между пересборками одного и того же исходника.
  ImageBase, EntryPoint, Subsystem, DllCharacteristics, ресурсы, TLS,
                  релокации — настройки образа, а не то, ради чего плагин
                  запускают. Половина этого набора хуже, чем ничего.
  IMAGE_DIRECTORY_ENTRY_DEBUG (→ pe.get_pdb_path()) — ВНУТРЕННИЙ ПУТЬ сборки:
                  абсолютный путь к PDB и каталогам исходников машины, на
                  которой файл собрали. Для файла из интернета это ещё и
                  утечка о чужой машине. Сниппет этот каталог вообще не
                  запрашивает у pefile — нечего отдавать даже по ошибке.
  imphash (pe.get_imphash()) — хеш списка импортов. Он и есть сигнатура
                  заражения, но и идентификатор; наружу не отдаётся.
  Полный pe.dump_dict() — «всё, кроме известного» означало бы, что любое
                  новое поле донора само собой утечёт в вывод плагина.

Поля копируются по списку ДОПУЩЕННЫХ имён, а не «всё, кроме небезопасного»:
новое поле донора не должно само собой появиться в выводе плагина.

Сеть: плагин объявляет network: []. Проверено измерением, а не рассуждением —
настоящий pefile запущен на фикстуре при заблокированных socket.connect,
socket.connect_ex, socket.create_connection, socket.getaddrinfo,
socket.gethostbyname (контроль блокировки в том же прогоне) и даёт те же 18
секций, те же 6 DLL и тот же exit 0. Донор — чистый разбор байтов, сетевых
путей у него нет.

Доменные ошибки (exit 1): empty_file, missing_file, not_pe, bad_file,
pefile_not_installed, timeout (retryable), no_report, tool_failed.
Платформенные (exit 2): битый JSON входа, wall_timeout не число/
неположительный/true-false, битый репорт донора, отсутствие parser_version
в нём, поля репорта не того типа.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile

DEFAULT_WALL = 300

# stderr донора — диагностика, а не результат. Предел нужен, чтобы донос не
# мог вылить в stderr плагина свой простырь: ядро кладёт limit 1 МБ на stderr
# плагина, и до него донос может довести свой вывод.
STDERR_LIMIT = 8000

DONOR_ERRORS = ("not_pe", "bad_file", "pefile_not_installed")

SNIPPET = '''\
import json
import sys


def bail(code, message):
    sys.stdout.write(json.dumps({"wedra_error": code,
                                 "wedra_message": message}))
    sys.exit(1)


try:
    import pefile
except ImportError as exc:
    bail("pefile_not_installed",
         "пакет pefile не импортируется: %s" % exc)

# Словари имён берутся У ДОНОРА (проверено на pefile 2024.8.26):
#   MACHINE_TYPE            — числовой код архитектуры -> имя (35 кодов);
#   SECTION_CHARACTERISTICS — однобитовый флаг секции -> имя (31 запись).
# Своих таблиц здесь нет намеренно: прежняя таблица обёртки знала 10
# архитектур и была второй правдой, которая устарела бы первой.
MACHINE_TYPE = getattr(pefile, "MACHINE_TYPE", None)
SECTION_CHARACTERISTICS = getattr(pefile, "SECTION_CHARACTERISTICS", None)


def machine_name(code):
    if not isinstance(MACHINE_TYPE, dict):
        return ""
    name = MACHINE_TYPE.get(code)
    return name if isinstance(name, str) else ""


def section_flags(mask):
    out = []
    if not isinstance(SECTION_CHARACTERISTICS, dict):
        return out
    for bit in sorted(k for k in SECTION_CHARACTERISTICS
                      if isinstance(k, int) and k > 0 and (k & (k - 1)) == 0):
        if mask & bit:
            out.append(SECTION_CHARACTERISTICS[bit])
    return out


def as_text(value):
    if isinstance(value, (bytes, bytearray)):
        return bytes(value).decode("utf-8", "replace")
    if value is None:
        return ""
    return str(value)


path = sys.argv[1]
try:
    pe = pefile.PE(path, fast_load=True)
except pefile.PEFormatError as exc:
    bail("not_pe", "файл не читается как PE: %s" % exc)
except Exception as exc:
    bail("bad_file", "не удалось разобрать PE: %r" % (exc,))

try:
    # Запрашивается РОВНО каталог импортов. Остальные (ресурсы, отладочная
    # информация, TLS, релокации) плагину не нужны, а отладочная информация
    # внутри PE несёт путь сборки машины автора.
    pe.parse_data_directories(
        directories=[pefile.DIRECTORY_ENTRY["IMAGE_DIRECTORY_ENTRY_IMPORT"]])
    sections = []
    for section in pe.sections:
        characteristics = int(section.Characteristics or 0)
        sections.append({
            "name": as_text(section.Name).split("\\x00")[0],
            "virtual_address": int(section.VirtualAddress),
            "size": int(section.SizeOfRawData or 0),
            "characteristics": characteristics,
            "flags": section_flags(characteristics),
        })
    imports = []
    for entry in getattr(pe, "DIRECTORY_ENTRY_IMPORT", []):
        symbols = []
        for symbol in entry.imports:
            # Импортировать можно по номеру, и тогда имени у символа нет.
            symbols.append({
                "name": as_text(symbol.name),
                "ordinal": int(symbol.ordinal) if symbol.ordinal else None,
            })
        imports.append({"dll": as_text(entry.dll), "symbols": symbols})
    report = {
        "parser_version": str(getattr(pefile, "__version__", "") or ""),
        "machine_id": int(pe.FILE_HEADER.Machine),
        "machine": machine_name(int(pe.FILE_HEADER.Machine)),
        "sections": sections,
        "imports": imports,
    }
    print(json.dumps(report, ensure_ascii=False))
finally:
    pe.close()
'''


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


class BadReport(Exception):
    """Репорт донора не отвечает контракту (плохие типы, нет обязательного
    поля). Это платформенная ошибка — exit 2, а не доменная."""


def extract_json(text):
    """Достаёт первый JSON-объект из stdout.

    Второе значение различает три случая, которые снаружи неразличимы:
    'ok'          — объект есть;
    'wrong_shape' — JSON разбирается, но это не объект (список, число,
                    строка): донор ответил, ответ негодный;
    'no_json'     — JSON-объекта в выводе нет вовсе.
    Первые два — bad_report (exit 2), последний — no_report.
    """
    try:
        obj = json.loads(text)
        if isinstance(obj, dict):
            return obj, "ok"
        return None, "wrong_shape"
    except ValueError:
        pass
    start = text.find("{")
    while start != -1:
        try:
            obj, _ = json.JSONDecoder().raw_decode(text, start)
            if isinstance(obj, dict):
                return obj, "ok"
            return None, "wrong_shape"
        except ValueError:
            pass
        start = text.find("{", start + 1)
    return None, "no_json"


def donor_command():
    """Команда запуска донора.

    По умолчанию — `python3 -c SNIPPET`: у pefile нет CLI, консольного скрипта
    он тоже не ставит, и «ищем бинарь в PATH» было бы поиском того, чего не
    существует. Модуль доступен ровно тогда, когда установлен пакет, то есть
    проверка «установлен ли донор» не врёт.

    PEFILE_BIN — штатный переключатель: путь или имя из PATH. Значение приходит
    из окружения хоста, поэтому попадает в argv как есть и дополнительно не
    цитируется: список аргументов не даёт инъекции.
    """
    bin_env = os.environ.get("PEFILE_BIN", "").strip()
    if not bin_env:
        return [sys.executable, "-c", SNIPPET]
    if "/" in bin_env or "\\" in bin_env:
        return [os.path.abspath(bin_env)]
    return [shutil.which(bin_env) or os.path.abspath(bin_env)]


def as_int(value, where):
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise BadReport("%s: ожидалось число, получено %r" % (where, value))
    if isinstance(value, float) and value != int(value):
        raise BadReport("%s: ожидалось целое, получено %r" % (where, value))
    return int(value)


def as_text(value, where):
    if value is None:
        return ""
    if not isinstance(value, str):
        raise BadReport("%s: ожидалась строка, получено %r" % (where, value))
    return value


def as_flag_list(value, where):
    if value is None:
        return []
    if not isinstance(value, list):
        raise BadReport("%s: ожидался массив, получено %r" % (where, value))
    return [as_text(item, where) for item in value]


def as_dict_list(value, where):
    if value is None:
        return []
    if not isinstance(value, list):
        raise BadReport("%s: ожидался массив, получено %r" % (where, value))
    for item in value:
        if not isinstance(item, dict):
            raise BadReport("%s: ожидался массив объектов, получен %r"
                            % (where, item))
    return value


def normalise(report):
    """Репорт донора → поля плагина.

    Собирается НОВЫЙ словарь из известных ключей, а не копируется репорт с
    вырезанным списком запрещённого: новое поле донора тогда не может само
    собой появиться в выводе плагина (см. раздел «Чего в выводе НЕТ»).
    """
    version = as_text(report.get("parser_version"), "parser_version").strip()
    if not version:
        # Версия парсера — часть ответа. Пустым полем не подменяется: неизвестно,
        # каким набором правил получены секции и импорты.
        raise BadReport("в репорте нет непустого строкового parser_version")

    machine_id = as_int(report.get("machine_id"), "machine_id")
    machine = as_text(report.get("machine"), "machine")

    sections = []
    for raw in as_dict_list(report.get("sections"), "sections"):
        name = as_text(raw.get("name"), "sections[].name")
        sections.append({
            "name": name,
            "virtual_address": as_int(raw.get("virtual_address"),
                                      "sections[%d].virtual_address" % len(sections)),
            "size": as_int(raw.get("size"), "sections[%d].size" % len(sections)),
            "characteristics": as_int(raw.get("characteristics"),
                                      "sections[%d].characteristics" % len(sections)),
            "flags": as_flag_list(raw.get("flags"),
                                  "sections[%d].flags" % len(sections)),
        })

    imports = []
    for raw in as_dict_list(report.get("imports"), "imports"):
        symbols = []
        for sym in as_dict_list(raw.get("symbols"),
                               "imports[%d].symbols" % len(imports)):
            ordinal = sym.get("ordinal")
            if ordinal is not None:
                ordinal = as_int(ordinal, "imports[].symbols[].ordinal")
            symbols.append({
                "name": as_text(sym.get("name"), "imports[].symbols[].name"),
                "ordinal": ordinal,
            })
        imports.append({"dll": as_text(raw.get("dll"), "imports[].dll"),
                        "symbols": symbols})

    return version, machine_id, machine, sections, imports


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

    # wall_timeout: отсутствие ключа и явный 0 — разные вещи. Наивный
    # `data.get("wall_timeout") or DEFAULT_WALL` путал их: переданный 0 молча
    # превращался в 300, и ран, просивший «без времени», отработал полный
    # лимит и отчитался ok. Ключа нет → дефолт; ключ есть и он неположителен
    # → ошибка входа.
    raw_wall = data.get("wall_timeout")
    if raw_wall is None:
        wall = float(DEFAULT_WALL)
    elif isinstance(raw_wall, bool):
        # JSON true — типовой дрейф на входе, а не «секунда».
        return fail("bad_wall_timeout",
                    "wall_timeout обязан быть числом, а не true/false",
                    exit_code=2)
    else:
        try:
            wall = float(raw_wall)
        except (TypeError, ValueError):
            return fail("bad_wall_timeout", "wall_timeout обязан быть числом",
                        exit_code=2)
        if wall <= 0:
            # Неположительный timeout ушёл бы в subprocess и немедленно вернул
            # TimeoutExpired — то есть плагин сообщил бы «timeout, retryable»
            # про вход, который повтором не чинится.
            return fail("bad_wall_timeout",
                        "wall_timeout обязан быть положительным числом",
                        exit_code=2)

    # Путь проверяет обёртка, а не донор: `pefile.PE("<каталог>")` падает с
    # OSError внутри донора, и без этой проверки «нет такого файла» и «файл
    # нечитаем» были бы одним и тем же словом.
    file_path = os.path.abspath(os.path.expanduser(target))
    if not os.path.isfile(file_path):
        return fail("missing_file",
                    "файл не найден или это не обычный файл: %s" % target)

    cmd = donor_command()
    if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
        # .py-мок запускаем через интерпретатор — прямой exec непереносим
        # (shebang+CRLF на Linux, ассоциации на Windows).
        cmd = [sys.executable] + cmd
    # Секретов в argv нет и быть не может: единственный аргумент — путь к
    # файлу, который оператор и так назвал сам.
    cmd += [file_path]

    # cwd — пустой временный каталог, а не каталог плагина. Донору не полагается
    # относительные пути, но cwd плагина для этого не годится: там лежат
    # main.py и фикстуры, и «случайно прочитанный сосед» — это утечка чужого
    # файла в отчёт, а не ошибка парсинга.
    with tempfile.TemporaryDirectory() as td:
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  encoding="utf-8", errors="replace",
                                  cwd=td, timeout=wall)
        except FileNotFoundError:
            return fail("pefile_not_installed",
                        "донор не запустился: pip install pefile==2024.8.26 "
                        "(или укажите PEFILE_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        "разбор PE не уложился в %.0fs: увеличьте wall_timeout "
                        "или возьмите файл меньше" % wall, retryable=True)
        stdout = proc.stdout or ""
        stderr = proc.stderr or ""

    if stderr:
        sys.stderr.write(stderr[:STDERR_LIMIT])

    report, shape = extract_json(stdout)

    # Ненулевой код возврата — отказ, даже если stdout успел напечатать
    # что-то похожее на репорт. Маркер доменной ошибки (not_pe/bad_file/
    # pefile_not_installed) разбираем отдельно: у него есть смысловой код, и
    # «формат не тот» и «файла нет» — разные вещи для оператора.
    if proc.returncode != 0:
        if isinstance(report, dict) and report.get("wedra_error") in DONOR_ERRORS:
            return fail(str(report["wedra_error"]),
                        str(report.get("wedra_message") or target))
        tail = stderr.strip().splitlines()
        last = tail[-1] if tail else ("exit %d" % proc.returncode)
        return fail("tool_failed",
                    "донор упал (exit %d): %s" % (proc.returncode, last))

    if not stdout.strip():
        return fail("no_report",
                    "донор не напечатал репорт в stdout (пустой вывод)")
    if report is None:
        if shape == "wrong_shape":
            return fail("bad_report",
                        "репорт pefile — не JSON-объект", exit_code=2)
        if "{" in stdout:
            return fail("bad_report",
                        "репорт pefile не разбирается как JSON", exit_code=2)
        return fail("no_report",
                    "донор не напечатал репорт в stdout (вывод без JSON)")

    try:
        version, machine_id, machine, sections, imports = normalise(report)
    except BadReport as exc:
        return fail("bad_report", str(exc), exit_code=2)

    # file возвращается ровно тем, что дал оператор. Абсолютная форма здесь
    # ничего бы не добавила, а привязала бы вывод и журнал рана к машине и к
    # каталогу запуска; проверяемый факт один — плагин открыл существующий
    # файл по этому входу (см. missing_file выше).
    return ok({
        "file": target,
        "machine": machine,
        "machine_id": machine_id,
        "sections": sections,
        "imports": imports,
        "section_count": len(sections),
        "import_dll_count": len(imports),
        "parser_version": version,
    })


if __name__ == "__main__":
    sys.exit(main())