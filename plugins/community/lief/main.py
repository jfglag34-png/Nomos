#!/usr/bin/env python3
"""lief — универсальный разбор бинарников PE/ELF/Mach-O (донор LIEF, паттерн C).

Вход (stdin JSON): file (путь к бинарнику, обязателен), wall_timeout (опц., 300).

Вызов: <LIEF_BIN|python3 -c SNIPPET> <абс.путь к файлу>  (cwd = временная папка).
SNIPPET — константа модуля: импортирует lief, зовёт общий парсер
lief.parse (а не lief.PE/lief.ELF/lief.MachO — назначение плагина именно общий
разбор) и печатает один JSON в stdout. main.py саму lief не импортирует (только
stdlib) и в сеть не ходит.

Что донор реально отдаёт (проверено на lief 1.0.0 на фикстуре
fixtures/sample.dll и на ELF-фикстуре соседнего плагина):

  * lief.__version__ = "1.0.0-d05b3499b" — строка донора как есть, с суффиксом
    сборки; вторая правда вида «1.0» была бы выдумкой обёртки;
  * b.format.name: "PE" / "ELF" / "MACHO" — имя из enum донора;
  * машина берётся из СВОЕГО заголовка каждого формата, потому что в lief 1.0
    абстрактных полей machine_type/is_64/endianness у бинарника больше нет
    (проверено: у lief._lief.PE.Binary нет machine_type, object_type, is_64,
    magic; у PE.Header есть machine, у ELF.Header — machine_type, у
    MachO.Header — cpu_type);
  * lief.parse возвращает None для файла, который не PE/ELF/Mach-O (проверено на
    текстовом файле), и объект даже для обрезанного PE — LIEF разбирает то, что
    успело, и врать про «битый файл» не даёт;
  * LIEF 1.0 на Windows НЕ ОТКРЫВАЕТ файл, если в пути есть не-ASCII символы:
    замер на пути репозитория (кириллица в имени пользователя) — lief.parse
    печатает «Failed to open …» и возвращает None. Короткое имя 8.3, полученное
    через GetShortPathNameW (только WinAPI, без внешних пакетов), открывается:
    замер — 18 секций. Поэтому перед запуском донора обёртка отдаёт ему путь в
    короткой форме, если путь не чисто ASCII, а наружу отдаёт строку оператора;
  * lief.parse ПИШЕТ В STDERR разбор COFF-символов при некоторых PE (замер:
    тысячи строк на обрезанном файле). Поэтому stderr донора обрезается
    пределом: иначе он съел бы stderr плагина, у которого ядро ставит 1 МБ.

Протокол донора в сниппете: exit 0 и JSON в stdout — успех; exit 1 и
{"wedra_error": "<код>", "wedra_message": "..."} в stdout — доменная ошибка
(unsupported_format, bad_file, lief_not_installed, version_unknown).

Выход (stdout JSON): {file, format, machine, entrypoint, imagebase, is_pie,
has_nx, virtual_size, sections[{name, size, memory_size, offset,
virtual_address, flags[]}], libraries[], section_count, parser_version}.

Чего в выводе НЕТ и почему (всё это LIEF умеет, и всё это сознательно не
читается):

  * authentihash_md5/sha1/sha256/sha512 — хеш-отпечаток файла, то есть ровно
    то, чем файл опознают без его чтения (аналог imphash в PE). Отдавать его
    «для сверки» нельзя: он и есть тот идентификатор, который мы не хотим
    выпускать дальше;
  * codeview_pdb — путь к PDB, то есть абсолютный путь с машины сборки;
  * debug_info — DWARF/CodeView: исходники, строки и пути сборки;
  * rich_header — отпечаток сборочной линейки (идентификатор инструментов, а не
    свойство файла);
  * header.time_date_stamps — время сборки: в выводе делает результат
    неповторяемым;
  * symbols — таблица имён, где встречаются имена исходных файлов сборки.

Доменные ошибки: empty_file, missing_file, unsupported_format, bad_file,
lief_not_installed, version_unknown, timeout (retryable), no_report,
tool_failed. Платформенные (exit 2): битый JSON входа, wall_timeout не
число/неположителен, нечитаемый stdout донора.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile

DEFAULT_WALL = 300

# stderr донора — диагностика, а не результат. Предел обязателен: LIEF 1.0 при
# разборе COFF-символов печатает в stderr тысячи строк (замер на обрезанном
# PE), а ядро кладёт на stderr плагина 1 МБ.
STDERR_LIMIT = 8000

SNIPPET = '''\
import json, sys

# LIEF печатает COFF-символы в stdout/stderr при некоторых PE, поэтому репорт
# ищется как ПОСЛЕДНИЙ объект в stdout, а не как весь stdout.
REPORT_MARK = "@@wedra-lief@@"


def bail(code, message):
    sys.stdout.write(json.dumps({"wedra_error": code,
                                 "wedra_message": message}))
    sys.exit(1)


def enum_name(value):
    """Имя из enum донора. Своей таблицы «код → имя» у обёртки нет."""
    name = getattr(value, "name", None)
    if name:
        return str(name)
    return str(value).rsplit(".", 1)[-1]


try:
    import lief
except ImportError as exc:
    bail("lief_not_installed", "пакет lief не импортируется: %s" % exc)

version = str(getattr(lief, "__version__", "") or "")
if not version:
    bail("version_unknown", "у LIEF нет ни __version__, ни метаданных пакета")

path = sys.argv[1]

# Магия формата нужна только для РАЗГОВОРА об ошибке: LIEF возвращает None и на
# «не PE/ELF/Mach-O», и на «не смог открыть путь» — это разные вещи для
# человека, а различать их больше нечем. Список магических чисел стандартный
# (PE: MZ; ELF: \\x7fELF; Mach-O: 4 шестибайтовых значения), и он ничего не
# добавляет к результату.
def looks_like_binary(head):
    if head[:2] == b"MZ" or head[:4] == b"PE\\x00\\x00":
        return "PE"
    if head[:4] == b"\\x7fELF":
        return "ELF"
    if head[:4] in (b"\\xfe\\xed\\xfa\\xce", b"\\xfe\\xed\\xfa\\xcf",
                    b"\\xce\\xfa\\xed\\xfe", b"\\xcf\\xfa\\xed\\xfe",
                    b"\\xca\\xfe\\xba\\xbe", b"\\xbe\\xba\\xfe\\xca"):
        return "Mach-O"
    return ""


try:
    with open(path, "rb") as probe:
        head = probe.read(4)
except OSError as exc:
    bail("bad_file", "не удалось прочитать файл: %s" % exc)

# Пути в сообщениях донора НЕ печатаются: путь к файлу уже знает вызывающий
# (это его строка), а абсолютная форма в тексте ошибки привязала бы сообщение к
# машине и выдала бы короткое имя 8.3, в котором ничего понятного нет.
try:
    binary = lief.parse(path)
except Exception as exc:
    bail("bad_file", "LIEF не смог разобрать файл: %r" % (exc,))
if binary is None:
    guessed = looks_like_binary(head)
    if guessed:
        bail("bad_file",
             "файл начинается как %s, но LIEF его не открыл или не разобрал; "
             "на Windows проверьте путь: LIEF 1.0 не открывает файлы, если в "
             "пути есть не-ASCII символы" % guessed)
    bail("unsupported_format",
         "файл не распознан как PE, ELF или Mach-O")

try:
    header = binary.header
    machine = ""
    for attr in ("machine", "machine_type", "cpu_type"):
        value = getattr(header, attr, None)
        if value is not None:
            machine = enum_name(value)
            break

    sections = []
    for section in binary.sections:
        # Флаги секции у каждого формата свои: у PE это
        # characteristics_lists, у ELF — flags_list. Имена берутся из
        # словарей донора; своего списка букв у обёртки нет.
        flags = getattr(section, "characteristics_lists", None)
        if flags is None:
            flags = getattr(section, "flags_list", None)
        if flags is None:
            flags = []
        size = int(section.size)
        virtual = getattr(section, "virtual_size", None)
        sections.append({
            "name": str(section.name or ""),
            "size": size,
            # У ELF поля virtual_size нет (там sh_size и есть размер), поэтому
            # memory_size = size, а не выдуманный ноль.
            "memory_size": int(virtual) if virtual is not None else size,
            "offset": int(section.offset),
            "virtual_address": int(section.virtual_address),
            "flags": [enum_name(f) for f in flags],
        })

    report = {
        "format": enum_name(binary.format),
        "machine": machine,
        "entrypoint": int(binary.entrypoint),
        "imagebase": int(binary.imagebase),
        "is_pie": bool(binary.is_pie),
        "has_nx": bool(binary.has_nx),
        "virtual_size": int(binary.virtual_size),
        "sections": sections,
        # libraries у PE — импортируемые DLL, у ELF — DT_NEEDED, у Mach-O —
        # подключаемые dylib. Это один и тот же список в API донора.
        "libraries": [str(name) for name in binary.libraries],
        "version": version,
    }
except Exception as exc:
    bail("bad_file", "разобранный файл не отдал содержимое: %r" % (exc,))

sys.stdout.write(REPORT_MARK + json.dumps(report))
'''

DONOR_ERRORS = ("unsupported_format", "bad_file", "lief_not_installed",
                "version_unknown")

REPORT_MARK = "@@wedra-lief@@"


def donor_error(report):
    if isinstance(report, dict) and report.get("wedra_error") in DONOR_ERRORS:
        return report
    return None


def donor_path(file_path):
    """Путь, который LIEF действительно откроет.

    Замер (Windows, lief 1.0.0): файл из репозитория, путь которого содержит
    кириллицу, не открывается — lief.parse печатает «Failed to open …» и
    возвращает None, то есть плагин сказал бы «это не PE». Короткое имя 8.3
    открывается (18 секций на фикстуре). Короткое имя берётся через
    GetShortPathNameW — это WinAPI из stdlib, никаких зависимостей.

    На других ОС и для чисто ASCII путей возвращается путь как есть.
    """
    try:
        file_path.encode("ascii")
        return file_path
    except UnicodeEncodeError:
        pass
    if os.name != "nt":
        return file_path
    try:
        import ctypes
        from ctypes import wintypes

        buffer = ctypes.create_unicode_buffer(4096)
        length = ctypes.windll.kernel32.GetShortPathNameW(
            wintypes.LPCWSTR(file_path), buffer, 4096)
        if length and buffer.value:
            short = buffer.value
            try:
                short.encode("ascii")
            except UnicodeEncodeError:
                return file_path
            if os.path.isfile(short):
                return short
    except Exception:
        pass
    return file_path


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


def resolve_cmd():
    """Команда запуска донора.

    По умолчанию — тот же интерпретатор с фиксированным сниппетом: у LIEF нет
    ни CLI, ни консольного скрипта (проверено: у пакета 1.0.0 в entry points
    пусто), и искать бинарь в PATH было бы поиском того, чего нет.

    LIEF_BIN — штатный переключатель (путь, имя из PATH или .py-мок).
    """
    bin_env = os.environ.get("LIEF_BIN", "").strip()
    if not bin_env:
        return [sys.executable, "-c", SNIPPET]
    if "/" not in bin_env and "\\" not in bin_env:
        cmd = [shutil.which(bin_env) or os.path.abspath(bin_env)]
    else:
        cmd = [os.path.abspath(bin_env)]
    if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
        cmd = [sys.executable] + cmd
    return cmd


def pick_dicts(value):
    return [item for item in (value or []) if isinstance(item, dict)]


def as_int(value):
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


def as_bool(value):
    return bool(value)


def extract_report(stdout):
    """(репорт|None, код платформенной ошибки|None).

    Репорт помечен маркером: LIEF в некоторых случаях пишет в stdout разбор
    символов, и «stdout целиком — это JSON» тогда ложь. Маркер отрезает шум.
    """
    text = stdout or ""
    marker_at = text.rfind(REPORT_MARK)
    payload = text[marker_at + len(REPORT_MARK):] if marker_at != -1 else text
    payload = payload.strip()
    if not payload:
        return None, "сниппет LIEF не напечатал JSON"
    try:
        report = json.loads(payload)
    except ValueError as exc:
        return None, "не разобран JSON-сниппет LIEF: %s" % exc
    if not isinstance(report, dict):
        return None, "неожиданный формат ответа LIEF"
    return report, None


def parse_wall(data):
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
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    target = str(data.get("file") or "").strip()
    if not target:
        return fail("empty_file", "file пуст")
    file_path = os.path.abspath(os.path.expanduser(target))
    if not os.path.isfile(file_path):
        return fail("missing_file", f"файл не найден: {target}")

    wall, wall_err = parse_wall(data)
    if wall_err:
        return fail("bad_wall_timeout", wall_err, exit_code=2)

    cmd = resolve_cmd() + [donor_path(file_path)]

    with tempfile.TemporaryDirectory() as td:
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  encoding="utf-8", errors="replace",
                                  cwd=td, timeout=wall)
        except FileNotFoundError:
            return fail("lief_not_installed",
                        "LIEF не запустился: pip install lief==1.0.0 "
                        "(или укажите LIEF_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"разбор LIEF не уложился в {wall:.0f}s: "
                        "увеличьте wall_timeout", retryable=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr[:STDERR_LIMIT])

        stdout = proc.stdout or ""
        if proc.returncode != 0:
            report, _ = extract_report(stdout)
            marker = donor_error(report)
            if marker:
                return fail(str(marker["wedra_error"]),
                            str(marker.get("wedra_message")
                                or f"файл не разобран: {target}"))
            tail = (proc.stderr or stdout or "").strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            return fail("tool_failed",
                        f"разбор LIEF вернул код {proc.returncode}: {last}")

        report, parse_err = extract_report(stdout)
        if report is None:
            if not stdout.strip():
                return fail("no_report", "сниппет LIEF не напечатал JSON")
            return fail("bad_report", parse_err, exit_code=2)

    marker = donor_error(report)
    if marker:
        return fail(str(marker["wedra_error"]),
                    str(marker.get("wedra_message")
                        or f"файл не разобран: {target}"))

    sections = [{"name": str(s.get("name") or ""),
                 "size": as_int(s.get("size")),
                 "memory_size": as_int(s.get("memory_size")),
                 "offset": as_int(s.get("offset")),
                 "virtual_address": as_int(s.get("virtual_address")),
                 "flags": [str(f) for f in (s.get("flags") or [])
                           if isinstance(f, (str, bytes))]}
                for s in pick_dicts(report.get("sections"))]
    libraries = [str(name) for name in (report.get("libraries") or [])
                 if isinstance(name, (str, bytes))]

    version = str(report.get("version") or "").strip()
    if not version:
        return fail("version_unknown",
                    "донор не назвал свою версию: разбор приписан неизвестной "
                    "версии LIEF", exit_code=2)

    return ok({"file": target,
               "format": str(report.get("format") or ""),
               "machine": str(report.get("machine") or ""),
               "entrypoint": as_int(report.get("entrypoint")),
               "imagebase": as_int(report.get("imagebase")),
               "is_pie": as_bool(report.get("is_pie")),
               "has_nx": as_bool(report.get("has_nx")),
               "virtual_size": as_int(report.get("virtual_size")),
               "sections": sections,
               "libraries": libraries,
               "section_count": len(sections),
               "parser_version": version})


if __name__ == "__main__":
    sys.exit(main())