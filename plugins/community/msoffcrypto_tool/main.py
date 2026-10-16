#!/usr/bin/env python3
"""msoffcrypto_tool — проверка и расшифровка офисного документа.

Вход (stdin JSON): file (путь к документу, обязателен), out (опц., куда
писать расшифрованный файл), wall_timeout (опц., 300).

Вызов: <MSOFFCRYPTO_TOOL_BIN|python3 -c SNIPPET> <абс.путь к файлу>
       [<абс.путь для расшифровки, может быть пустым>]   (cwd = временная папка).
SNIPPET — константа модуля: импортирует msoffcrypto, печатает один JSON в
stdout. Пароль донора передаётся НЕ аргументом, а его stdin (JSON
{"password": ...}): единственный способ CLI msoffcrypto-tool принять пароль —
флаг -p, то есть argv, а argv виден в списке процессов любому пользователю
машины. Пароль приходит только из env MSOFFCRYPTO_PASSWORD и в вывод не
попадает.

Что донор реально отдаёт (проверено на msoffcrypto-tool 6.0.0 и на фикстурах
из fixtures/, собранных скриптом make_office.py):

    encrypted.docx  is_encrypted() → True,  office.type == "agile"
                    после load_key+decrypt открытый текст — байт-в-байт
                    та же plain.docx, что шифровали
    plain.docx      is_encrypted() → False, office.type == "plain"
    plain.txt       OfficeFile() → FileFormatError «Unsupported file format»

Тот же CLI на тех же файлах печатает в stderr (проверено):
    Version: 6.0.0 / OOXMLFile.type: agile / encrypted.docx: encrypted /
    totalSize: 35321 — то есть значения, которые обёртка отдаёт наружу, взяты
из ответа донора, а не придуманы обёрткой. Словарь типов шифрования тоже его:
agile, standard, extensible (OOXML), xor, rc4, rc4_cryptoapi (xls/doc/ppt).

Выход (stdout JSON): {file, encrypted, encryption_type, decrypted,
decrypted_size, tool_version}. file — строка оператора, абсолютная форма
ничего бы не добавила, а привязала бы вывод и журнал рана к машине.

Чего в выводе НЕТ и почему:

  * office.info — это соль, encryptedKeyValue, encryptedVerifierHashValue,
    keyDataSalt, encryptedHmacValue: проверочные хеши пароля и ключевой
    материал. Для слабого пароля они перебираются офлайн (ровно тот же
    класс проблемы, что hashed_secret у detect_secrets), поэтому наружу не
    отдаётся ничего из info, а поля копируются по списку ДОПУЩЕННЫХ имён —
    новое поле донора не должно само собой утекать в вывод плагина.
  * office.secret_key — производная от пароля, тот же случай.
  * Путь расшифрованного файла. `out` задаёт, КУДА писать, но абсолютный
    путь наружу не отдаётся: это внутренний идентификатор машины, по нему в
    ран попадает путь хоста. Поэтому `out` без каталога кладёт файл во
    временный каталог ОС — и, что важнее, НЕ рядом с main.py: запись внутри
    каталога плагина меняет хэш его содержимого, и следующий запуск получил бы
    отказ «не в allow-list доверия ядра» (вплоть до красных `wedra plugin test`
    на этом же плагине). Каталог с результатом не убираем: файл должен
    дожить до конца прогона, даже если путь к нему никто не узнает.
  * Сам пароль — ни в выводе, ни в сообщениях об ошибках (в тексте ошибок
    донора его нет, но проверка на это остаётся: сообщение обрезается по
    stderr, куда донор мог бы что-то напечатать).

Сеть: плагин объявляет network: []. Донор читает только файл: проверено
запуском с заблокированными socket.connect/create_connection/getaddrinfo —
тот же результат и тот же exit-код сниппета.

Доменные ошибки: empty_file, missing_file, unsupported_format, bad_file,
not_encrypted, bad_password, msoffcrypto_tool_not_installed, version_unknown,
timeout (retryable), no_report, tool_failed. Платформенные (exit 2): битый
JSON входа, out не строка, wall_timeout не число/неположителен, нечитаемый
ответ донора.
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

# Поля ответа донора, которые разрешено копировать наружу. Список, а не
# запрет: см. пояснение к office.info в модульной документации. Наружу идут
# encrypted, encryption_type, decrypted, decrypted_size и version — ровно те,
# что перечислены в DONOR_ERRORS-обёртке; всё остальное (office.info,
# office.secret_key) не копируется никогда.
REPORT_FIELDS = ("encrypted", "encryption_type", "decrypted",
                 "decrypted_size", "version")

SNIPPET = '''\
import importlib.metadata
import io
import json
import os
import sys


def bail(code, message):
    sys.stdout.write(json.dumps({"wedra_error": code,
                                 "wedra_message": message}))
    sys.exit(1)


try:
    import msoffcrypto
    from msoffcrypto import exceptions
except ImportError as exc:
    bail("msoffcrypto_tool_not_installed",
         "пакет msoffcrypto-tool не импортируется: %s" % exc)

# Версия донора — часть ответа: «документ не зашифрован» и «эта версия не
# знает такой тип шифрования» суть разные вещи, и без версии они сливаются.
try:
    version = str(importlib.metadata.version("msoffcrypto-tool") or "")
except Exception:
    version = ""
if not version:
    bail("version_unknown", "у msoffcrypto-tool нет метаданных версии")

path = sys.argv[1]
out_path = sys.argv[2] if len(sys.argv) > 2 else ""

# Пароль приходит по stdin донора, а не в argv.
password = ""
try:
    request = json.load(sys.stdin)
    if isinstance(request, dict):
        password = str(request.get("password") or "")
except Exception:
    password = ""

handle = None
try:
    try:
        handle = open(path, "rb")
    except OSError as exc:
        bail("bad_file", "не удалось открыть файл: %s" % exc)

    try:
        office = msoffcrypto.OfficeFile(handle)
    except exceptions.FileFormatError as exc:
        bail("unsupported_format", str(exc) or "документ не офисный")
    except Exception as exc:
        bail("bad_file", "не удалось разобрать документ: %r" % (exc,))

    try:
        encrypted = bool(office.is_encrypted())
    except Exception as exc:
        bail("bad_file", "не удалось определить шифрование: %r" % (exc,))

    encryption_type = str(getattr(office, "type", "") or "")

    decrypted = False
    decrypted_size = 0
    if out_path:
        # Расшифровка только по явному out: файл, который никто не сможет
        # найти, был бы хуже, чем его отсутствие.
        if not encrypted:
            bail("not_encrypted", "документ не зашифрован: расшифровывать нечего")
        try:
            office.load_key(password=password)
            # Расшифровываем в память и пишем файл только после успеха: донор
            # проверяет пароль по результату, и запись «на месте» оставила бы
            # пустой файл там, где оператор ждёт документ.
            sink = io.BytesIO()
            office.decrypt(sink)
        except exceptions.InvalidKeyError as exc:
            bail("bad_password", str(exc) or "пароль не подошёл")
        except exceptions.DecryptionError as exc:
            bail("not_encrypted", str(exc) or "документ не зашифрован")
        except OSError as exc:
            bail("bad_file", "не удалось записать результат: %s" % exc)
        except Exception as exc:
            bail("bad_file", "не удалось расшифровать: %r" % (exc,))
        try:
            with open(out_path, "wb") as target:
                target.write(sink.getvalue())
        except OSError as exc:
            bail("bad_file", "не удалось записать результат: %s" % exc)
        try:
            decrypted_size = os.path.getsize(out_path)
        except OSError:
            decrypted_size = 0
        if decrypted_size == 0:
            bail("bad_file", "донор записал пустой результат")
        decrypted = True

    # Копируем по списку разрешённых имён: office.info и office.secret_key
    # наружу не идут никогда (см. модульную документацию).
    print(json.dumps({
        "encrypted": encrypted,
        "encryption_type": encryption_type,
        "decrypted": decrypted,
        "decrypted_size": decrypted_size,
        "version": version,
    }))
finally:
    if handle is not None:
        handle.close()
'''

DONOR_ERRORS = ("unsupported_format", "bad_file", "not_encrypted",
                "bad_password", "msoffcrypto_tool_not_installed",
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


def resolve_cmd():
    """Команда запуска донора.

    По умолчанию — тот же интерпретатор с фиксированным сниппетом: у пакета
    есть console_script msoffcrypto-tool, но он живёт в каталоге Scripts, а
    тот на Windows часто не в PATH, и тогда «донор установлен и при этом не
    найден».

    MSOFFCRYPTO_TOOL_BIN — штатный переключатель (путь, имя из PATH или
    .py-мок).
    """
    bin_env = os.environ.get("MSOFFCRYPTO_TOOL_BIN", "").strip()
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


def parse_wall(data):
    """(секунды|None, код сообщения|None).

    Отсутствие ключа и явный 0 — разные вещи: наивный
    `data.get("wall_timeout") or DEFAULT_WALL` превращал 0 в 300, то есть ран
    просил «без времени», а плагин отработал полный лимит и сообщил ok.
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


def as_bool(value):
    """bool из ответа донора. Мусор вместо bool — False, а не исключение:
    иначе плагин упал бы с platform:crash на неожиданном типе поля."""
    if isinstance(value, bool):
        return value
    return str(value or "").strip().lower() in ("true", "1", "yes")


def as_int(value):
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


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

    raw_out = data.get("out")
    if raw_out is not None and not isinstance(raw_out, str):
        return fail("bad_out", "out обязан быть строкой", exit_code=2)
    out = (raw_out or "").strip()

    wall, wall_err = parse_wall(data)
    if wall_err:
        return fail("bad_wall_timeout", wall_err, exit_code=2)

    file_path = os.path.abspath(os.path.expanduser(target))
    if not os.path.isfile(file_path):
        # Путь проверяет обёртка: иначе «нет такого файла» и «файл не офисный»
        # были бы одним и тем же словом с кодом от донора.
        return fail("missing_file", "файл не найден: %s" % target)

    # Пароль — только из env. В argv он не попадает никогда: вход плагина
    # уходит в контекст рана и в журнал, а argv донора виден в списке процессов.
    password = os.environ.get("MSOFFCRYPTO_PASSWORD", "")

    out_path = ""
    if password and out:
        if os.path.isabs(os.path.expanduser(out)):
            out_path = os.path.abspath(os.path.expanduser(out))
            parent = os.path.dirname(out_path)
            if parent and not os.path.isdir(parent):
                try:
                    os.makedirs(parent, exist_ok=True)
                except OSError as e:
                    return fail("bad_out", "не создать каталог для out: %s" % e,
                                exit_code=2)
        else:
            # Относительный out кладём во временный каталог ОС, а не рядом с
            # main.py. Причина не в удобстве: запись внутри каталога плагина
            # меняет его хэш содержимого, и следующий же запуск получил бы отказ
            # «не в allow-list доверия ядра» — вплоть до красных
            # `wedra plugin test` на ровно этом плагине. Каталог не убираем:
            # путь наружу не отдаётся (см. модульную документацию), а файл
            # должен дожить до конца прогона.
            out_path = os.path.join(
                tempfile.mkdtemp(prefix="wedra_msoffcrypto_"),
                os.path.basename(out))

    cmd = resolve_cmd() + [file_path, out_path]

    with tempfile.TemporaryDirectory() as td:
        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  encoding="utf-8", errors="replace",
                                  cwd=td, timeout=wall,
                                  input=json.dumps({"password": password}))
        except FileNotFoundError:
            return fail("msoffcrypto_tool_not_installed",
                        "msoffcrypto-tool не найден: "
                        "pip install msoffcrypto-tool==6.0.0 "
                        "(или укажите MSOFFCRYPTO_TOOL_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        "разбор msoffcrypto-tool не уложился в %.0fs: "
                        "уменьшите размер документа или увеличьте wall_timeout"
                        % wall, retryable=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr[:STDERR_LIMIT])

        report = parse_json_blob(proc.stdout or "")
        if proc.returncode != 0:
            marker = donor_error(report)
            if marker:
                return fail(str(marker["wedra_error"]),
                            str(marker.get("wedra_message") or target))
            tail = (proc.stderr or proc.stdout or "").strip().splitlines()
            last = tail[-1] if tail else "exit %d" % proc.returncode
            return fail("tool_failed",
                        "разбор документа вернул код %d: %s"
                        % (proc.returncode, last))
        if not (proc.stdout or "").strip():
            return fail("no_report", "сниппет msoffcrypto-tool не напечатал JSON")
        if report is None:
            return fail("bad_report",
                        "не разобран JSON-сниппет msoffcrypto-tool",
                        exit_code=2)

    marker = donor_error(report)
    if marker:
        return fail(str(marker["wedra_error"]),
                    str(marker.get("wedra_message") or target))
    if not isinstance(report, dict):
        return fail("bad_report", "неожиданный формат ответа msoffcrypto-tool",
                    exit_code=2)

    # Версия донора обязательна: без неё «не зашифровано» неотличимо от
    # «эта версия не знает такой тип шифрования». Пустой строкой не подменить.
    version = str(report.get("version") or "").strip()
    if not version:
        return fail("version_unknown",
                    "донор не назвал свою версию: документ разобран "
                    "неизвестной версией msoffcrypto-tool", exit_code=2)

    # Копируем по списку ДОПУЩЕННЫХ имён, а не «всё, кроме известного»: новое
    # поле донора не должно само собой утекать в вывод плагина.
    picked = {field: report.get(field) for field in REPORT_FIELDS}
    result = {"file": target,
              "encrypted": as_bool(picked.get("encrypted")),
              "encryption_type": str(picked.get("encryption_type") or ""),
              "decrypted": as_bool(picked.get("decrypted")),
              "decrypted_size": as_int(picked.get("decrypted_size")),
              "tool_version": version}
    if out_path and not result["decrypted"]:
        # Донор сказал «не расшифровал», а оператор ждал файл по out.
        return fail("no_report",
                    "донор не расшифровал документ, а out задан", exit_code=2)
    return ok(result)


if __name__ == "__main__":
    sys.exit(main())
