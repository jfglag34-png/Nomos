#!/usr/bin/env python3
"""pyelftools — разбор ELF-файла (донор elftools: библиотека без CLI, паттерн C).

Вход (stdin JSON): file (путь к ELF-файлу, обязателен), wall_timeout (опц., 300).

Вызов: <PYELFTOOLS_BIN|python3 -c SNIPPET> <абс.путь к файлу>  (cwd = временная
папка). SNIPPET — константа модуля: импортирует elftools, читает класс, тип,
машину, entry, секции и теги DT_NEEDED, печатает один JSON в stdout. main.py
сам elftools не импортирует (только stdlib) и в сеть не ходит.

Что донор реально отдаёт (проверено на pyelftools 0.33 и на трёх фикстурах из
fixtures/, собранных скриптом make_elf.py):

  libfixture.so   ELF64 ET_DYN EM_X86_64, 8 секций, DT_NEEDED libc.so.6 и
                  libpthread.so.0, elftools.__version__ = "0.33";
  fixture32.o     ELF32 ET_REL EM_386, 3 секции, DT_NEEDED нет;
  truncated.so    ELFParseError — заголовок обещает таблицу секций за концом
                  файла.

Протокол донора в сниппете: exit 0 и JSON в stdout — успех; exit 1 и
{"wedra_error": "<код>", "wedra_message": "..."} в stdout — доменная ошибка
(not_elf, bad_file, pyelftools_not_installed, version_unknown).

Выход (stdout JSON): {file, elf_class, elf_type, machine, entry, sections
[{name, type, flags, size, addr}], libraries[], section_count, parser_version}.

Имена полей взяты из словарей ДОНОРА, а не из таблиц обёртки: sh_type
отдаётся как "SHT_PROGBITS"/"SHT_DYNAMIC", e_type как "ET_DYN"/"ET_REL",
e_machine как "EM_X86_64"/"EM_386". Пустая строка означала бы «эта версия
донора не знает такой код» — числовой код при этом остался бы в файле, и «не
знаю» с «нечем» не путались бы.

Чего в выводе НЕТ и почему:

  * Символы (.symtab/.dynsym) не собираются вообще. Имя символа типа STT_FILE —
    это путь к исходнику на машине сборки, то есть внутренний идентификатор
    сборки; таблица символов целиком для задачи «разобрать образ» не нужна.
  * .note.gnu.build-id не читается: это хеш-отпечаток файла (то же, что
    imphash в PE), и по нему образ опознаётся без содержимого.
  * Абсолютный путь: наружу отдаётся строка file ровно та, что дал оператор.
    Путь донора не печатается — сниппет отдаёт только разобранное.
  * flags у секции отдаётся ЧИСЛОМ (sh_flags как есть). У pyelftools нет
    словаря «бит → имя» для секций (SH_FLAGS — это набор констант, а не карта),
    поэтому разложение на «A», «W», «X» было бы второй правдой обёртки.

Доменные ошибки: empty_file, missing_file, not_elf, bad_file,
pyelftools_not_installed, version_unknown, timeout (retryable), no_report,
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

# stderr донора — диагностика, а не результат; предел нужен, чтобы донос не
# вылил в stderr плагина свой простырь (ядро кладёт на него 1 МБ).
STDERR_LIMIT = 8000

SNIPPET = '''\
import json, sys

import importlib.metadata


def bail(code, message):
    sys.stdout.write(json.dumps({"wedra_error": code,
                                 "wedra_message": message}))
    sys.exit(1)


try:
    import elftools
    from elftools.elf.elffile import ELFFile
    from elftools.common.exceptions import ELFError, ELFParseError
except ImportError as exc:
    bail("pyelftools_not_installed",
         "пакет pyelftools не импортируется: %s" % exc)

# Версия донора — его собственный атрибут. Без неё «секций нет» неотличимо от
# «эта версия не разобрала таблицу секций».
version = str(getattr(elftools, "__version__", "") or "")
if not version:
    try:
        version = importlib.metadata.version("pyelftools")
    except Exception:
        version = ""
if not version:
    bail("version_unknown", "у pyelftools нет ни __version__, ни метаданных пакета")

path = sys.argv[1]
try:
    handle = open(path, "rb")
except OSError as exc:
    bail("bad_file", "не удалось открыть файл: %s" % exc)

try:
    try:
        elf = ELFFile(handle)
    except ELFParseError as exc:
        # Магия ELF на месте, а дальше файл не сошёлся: обрезан или побит.
        handle.close()
        bail("bad_file", "файл начинается как ELF, но не разбирается: %s" % exc)
    except ELFError as exc:
        handle.close()
        bail("not_elf", "файл не читается как ELF: %s" % exc)
    except Exception as exc:
        handle.close()
        bail("bad_file", "не удалось разобрать ELF: %r" % (exc,))

    try:
        sections = []
        libraries = []
        for section in elf.iter_sections():
            header = section.header
            sections.append({
                "name": str(section.name or ""),
                "type": str(header["sh_type"]),
                "flags": int(header["sh_flags"]),
                "size": int(header["sh_size"]),
                "addr": int(header["sh_addr"]),
            })
            # DT_NEEDED лежит в тегах секции типа SHT_DYNAMIC. Секций с
            # тегами может быть несколько (.dynamic и .dynamic.lds) — берём
            # все и не повторяемся.
            iter_tags = getattr(section, "iter_tags", None)
            if iter_tags is not None:
                for tag in iter_tags():
                    needed = getattr(tag, "needed", None)
                    if needed and needed not in libraries:
                        libraries.append(str(needed))
    except Exception as exc:
        handle.close()
        bail("bad_file", "секции не прочитались: %r" % (exc,))

    print(json.dumps({
        "class": "ELF%d" % int(elf.elfclass),
        "type": str(elf.header["e_type"]),
        "machine": str(elf.header["e_machine"]),
        "entry": int(elf.header["e_entry"]),
        "sections": sections,
        "imported_libraries": libraries,
        "version": version,
    }))
finally:
    handle.close()
'''

DONOR_ERRORS = ("not_elf", "bad_file", "pyelftools_not_installed",
                "version_unknown")


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

    По умолчанию — тот же интерпретатор с фиксированным сниппетом: у pyelftools
    нет ни CLI, ни консольного скрипта, и искать бинарь в PATH было бы поиском
    того, чего нет (проверено: у пакета 0.33 нет ни одного entry point).

    PYELFTOOLS_BIN — штатный переключатель (путь, имя из PATH или .py-мок).
    """
    bin_env = os.environ.get("PYELFTOOLS_BIN", "").strip()
    if not bin_env:
        return [sys.executable, "-c", SNIPPET]
    if "/" not in bin_env and "\\" not in bin_env:
        cmd = [shutil.which(bin_env) or os.path.abspath(bin_env)]
    else:
        cmd = [os.path.abspath(bin_env)]
    if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
        # .py-мок запускаем через интерпретатор: прямой exec непереносим
        # (shebang+CRLF на Linux, ассоциации на Windows).
        cmd = [sys.executable] + cmd
    return cmd


def parse_json_blob(text):
    stripped = (text or "").strip()
    if not stripped:
        return None
    try:
        return json.loads(stripped)
    except ValueError:
        return None


def donor_error(report):
    if isinstance(report, dict) and report.get("wedra_error") in DONOR_ERRORS:
        return report
    return None


def pick_dicts(value):
    return [item for item in (value or []) if isinstance(item, dict)]


def as_int(value):
    """Целое из ответа донора. Мусор вместо числа — 0, а не исключение:
    иначе плагин упал бы с platform:crash на неожиданном типе поля."""
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


def parse_wall(data):
    """(секунды|None, код платформенной ошибки|None).

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
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    target = str(data.get("file") or "").strip()
    if not target:
        return fail("empty_file", "file пуст")
    # Путь проверяет обёртка: без этого «нет такого файла» и «файл нечитаем»
    # были бы одним и тем же словом с кодом от донора.
    file_path = os.path.abspath(os.path.expanduser(target))
    if not os.path.isfile(file_path):
        return fail("missing_file", f"файл не найден: {target}")

    wall, wall_err = parse_wall(data)
    if wall_err:
        return fail("bad_wall_timeout", wall_err, exit_code=2)

    cmd = resolve_cmd() + [file_path]

    with tempfile.TemporaryDirectory() as td:
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  encoding="utf-8", errors="replace",
                                  cwd=td, timeout=wall)
        except FileNotFoundError:
            return fail("pyelftools_not_installed",
                        "pyelftools не запустился: pip install pyelftools==0.33 "
                        "(или укажите PYELFTOOLS_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"разбор pyelftools не уложился в {wall:.0f}s: "
                        "увеличьте wall_timeout", retryable=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr[:STDERR_LIMIT])

        stdout = proc.stdout or ""
        report = parse_json_blob(stdout)
        if proc.returncode != 0:
            marker = donor_error(report)
            if marker:
                return fail(str(marker["wedra_error"]),
                            str(marker.get("wedra_message") or target))
            tail = (proc.stderr or stdout or "").strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            return fail("tool_failed",
                        f"разбор ELF вернул код {proc.returncode}: {last}")
        if not stdout.strip():
            return fail("no_report", "сниппет pyelftools не напечатал JSON")
        if report is None:
            return fail("bad_report", "не разобран JSON-сниппет pyelftools",
                        exit_code=2)

    marker = donor_error(report)
    if marker:
        return fail(str(marker["wedra_error"]),
                    str(marker.get("wedra_message") or target))
    if not isinstance(report, dict):
        return fail("bad_report", "неожиданный формат ответа pyelftools",
                    exit_code=2)

    sections = [{"name": str(s.get("name") or ""),
                 "type": str(s.get("type") or ""),
                 "flags": as_int(s.get("flags")),
                 "size": as_int(s.get("size")),
                 "addr": as_int(s.get("addr"))}
                for s in pick_dicts(report.get("sections"))]
    libraries = [str(lib) for lib in (report.get("imported_libraries") or [])
                 if isinstance(lib, (str, bytes))]

    # Версия донора — часть ответа, а не украшение: разбор без указания версии
    # неотличим от разбора другой версии. Пустой строкой это не подменить.
    version = str(report.get("version") or "").strip()
    if not version:
        return fail("version_unknown",
                    "донор не назвал свою версию: разбор приписан неизвестной "
                    "версии pyelftools", exit_code=2)

    return ok({"file": target,
               "elf_class": str(report.get("class") or ""),
               "elf_type": str(report.get("type") or ""),
               "machine": str(report.get("machine") or ""),
               "entry": as_int(report.get("entry")),
               "sections": sections,
               "libraries": libraries,
               "section_count": len(sections),
               "parser_version": version})


if __name__ == "__main__":
    sys.exit(main())