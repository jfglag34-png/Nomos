#!/usr/bin/env python3
"""altdns — генерация перестановок поддоменов (обёртка над CLI altdns).

Вход (stdin JSON): host, words[] (опц.), number_suffix (опц., bool),
wall_timeout (опц., 300).

Вызов: <ALTDNS_BIN|python3 -m altdns> -i <tmp>/subdomains.txt
       -o <tmp>/permutations.txt -w <tmp>/words.txt [-n]
       (cwd = временная папка, та же и таймаут).

Что донор реально отдаёт (проверено на py-altdns 1.0.2):

  * stdout/stderr на успехе ПУСТЫ (замер: 0 байт в обоих), exit 0;
  * результат лежит в файле -o, по одному «altered subdomain» в строке;
  * перестановок для dev.example.com и словаря [api, www] ровно 12:
    {api,www}-{api,www}.example.com, api.{www,dev}..., dev-www, wwwdev и т.д.;
  * для хоста БЕЗ субдомена (example.com) результат не пустой, а состоит из
    dev.example.com — то есть «пусто» и «хост не субдомен» не совпадают;
  * порядок строк НЕ детерминирован: донор дедуплицирует через set() и печатает
    в порядке обхода множества, который зависит от хэша имён (PYTHONHASHSEED).
    Обёртка поэтому сортирует: иначе один и тот же вход давал бы разные массивы.

Выход (stdout JSON): {host, permutations[], total, generator_version}.

generator_version — версия ПАКЕТА донора из importlib.metadata, а не «какая-то»
строка: у altdns нет флага --version, печатать нечего, а перестановки зависят от
версии и от снимка public suffix list внутри tldextract. Без версии «перестановок
нет» неотличимо от «эта версия ничего не генерирует».

Чего в выводе НЕТ и почему: у altdns кроме имён перестановок ничего нет —
ни хешей, ни путей, ни идентификаторов. Абсолютный путь временного каталога,
который донор пишет в stderr («[*] Wrote 12 altered subdomains to …»), в stderr
плагина НЕ попадает: он заменяется на <tmp>, иначе журнал рана был бы привязан к
машине.

Сеть: манифест объявляет network 443 — и это не формальность. Проверено: внутри
altdns используется tldextract, который при первом обращении тянет список
public suffix с publicsuffix.org. Замер: с заблокированными socket.connect
вызов altdns доходит до попытки соединения с publicsuffix.org. При недоступной
сети tldextract откатывается на встроенный снимок списка — замер: тот же вход и
те же 12 перестановок при соединениях, падающих с OSError. Резолв (-r/-s)
обёртка не включает никогда: DNS-запросов донор не делает.

Доменные ошибки: empty_host, altdns_not_installed, timeout (retryable),
no_report, tool_failed. Платформенные (exit 2): битый JSON входа, words не
массив, number_suffix не bool, wall_timeout не число/неположителен, отчёт
не читается как UTF-8, недоступна версия пакета донора.
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
from importlib import metadata

DEFAULT_WALL = 300

# stderr донора — диагностика, а не результат. Предел нужен, чтобы донос не
# вылил в stderr плагина свой простырь: ядро кладёт 1 МБ на stderr плагина, и
# донос может довести свой вывод до этого предела.
STDERR_LIMIT = 8000

# Словарь по умолчанию — ПОЛИТИКА ОБЁРТКИ, а не донора: у altdns ключ -w
# обязателен, своего словаря у него нет. Значит перестановки «без words» не
# можно предъявить донору — их негде взять. Перечень поэтому короткий и
# неизменяемый, а тест его пиннит, чтобы «сменился дефолт» было видно.
DEFAULT_WORDS = ["admin", "api", "beta", "demo", "dev", "qa", "stage",
                 "test"]

DONOR_DIST = "py-altdns"  # имя пакета на PyPI; модуль называется altdns
DONOR_MODULE = "altdns"


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


def donor_command():
    """Команда запуска донора.

    По умолчанию — `python3 -m altdns`, а не console-script `altdns`: pip кладёт
    скрипты в каталог Scripts (на Windows), а он часто не в PATH — и тогда донор
    установлен и одновременно «не найден». Модуль доступен ровно тогда, когда
    установлен пакет, то есть проверка «установлен ли донор» не врёт.

    ALTDNS_BIN — штатный переключатель: путь или имя из PATH (нужно мокам и
    нестандартным установкам). Значение приходит из окружения хоста и попадает
    в argv как есть; список аргументов инъекции не даёт.
    """
    bin_env = os.environ.get("ALTDNS_BIN", "").strip()
    if not bin_env:
        return [sys.executable, "-m", DONOR_MODULE]
    if "/" not in bin_env and "\\" not in bin_env:
        return [shutil.which(bin_env) or os.path.abspath(bin_env)]
    return [os.path.abspath(bin_env)]


def donor_version():
    """Версия пакета донора. None — определить нечем.

    Источник тот же, что у pip: метаданные установленного пакета. Атрибута
    __version__ у altdns нет, флага --version тоже, а перестановки зависят от
    версии — поэтому версия часть ответа, а не украшение.
    """
    for dist in (DONOR_DIST, DONOR_MODULE):
        try:
            return metadata.version(dist)
        except metadata.PackageNotFoundError:
            continue
    return None


def read_lines(path):
    with open(path, encoding="utf-8") as f:
        return [line.strip() for line in f if line.strip()]


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

    host = str(data.get("host") or "").strip().lower().rstrip(".")
    if not host:
        return fail("empty_host", "host пуст")

    words = data.get("words")
    if words is None:
        words = list(DEFAULT_WORDS)
    if not isinstance(words, list):
        return fail("bad_words", "words обязан быть массивом", exit_code=2)
    words = [str(w).strip().lower() for w in words if str(w).strip()]

    number_suffix = data.get("number_suffix")
    if number_suffix is None:
        number_suffix = False
    if not isinstance(number_suffix, bool):
        return fail("bad_number_suffix", "number_suffix обязан быть bool",
                    exit_code=2)

    # wall_timeout: отсутствие ключа и явный 0 — разные вещи. Наивный
    # `data.get("wall_timeout") or DEFAULT_WALL` путал их, и переданный 0 молча
    # превращался в 300: ран просил «без времени», а плагин отработал полный
    # лимит и отчитался ok.
    raw_wall = data.get("wall_timeout")
    if raw_wall is None:
        wall = float(DEFAULT_WALL)
    elif isinstance(raw_wall, bool):
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

    version = donor_version()
    if not version:
        # Донор запускается, а пакет с метаданными не найден: установка
        # неполная (например, модуль подложен в PYTHONPATH). Перестановки были
        # бы приписаны версии, которой нет, — не выдаём.
        return fail("version_unknown",
                    f"не удалось определить версию пакета {DONOR_DIST}: "
                    f"установите донора через pip (pip install {DONOR_DIST})",
                    exit_code=2)

    cmd = donor_command()
    if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
        # .py-мок запускаем через интерпретатор: прямой exec непереносим
        # (shebang+CRLF на Linux, ассоциации на Windows).
        cmd = [sys.executable] + cmd

    with tempfile.TemporaryDirectory() as td:
        subdomains_path = os.path.join(td, "subdomains.txt")
        words_path = os.path.join(td, "words.txt")
        out_path = os.path.join(td, "permutations.txt")
        with open(subdomains_path, "w", encoding="utf-8") as f:
            f.write(host + "\n")
        with open(words_path, "w", encoding="utf-8") as f:
            f.write("\n".join(words) + ("\n" if words else ""))

        cmd += ["-i", subdomains_path, "-o", out_path, "-w", words_path]
        if number_suffix:
            cmd += ["-n"]

        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  encoding="utf-8", errors="replace",
                                  cwd=td, timeout=wall)
        except FileNotFoundError:
            return fail("altdns_not_installed",
                        "altdns не найден: pip install py-altdns==1.0.2 "
                        "(или укажите ALTDNS_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"altdns не уложился в {wall:.0f}s: уменьшите words "
                        "или увеличьте wall_timeout",
                        retryable=True)

        # Путь временного каталога в stderr донора заменяем на <tmp>: он
        # привязывает журнал рана к машине и ничего не сообщает о доноре.
        noise = ((proc.stderr or "") + (proc.stdout or "")).replace(td, "<tmp>")
        if noise:
            sys.stderr.write(noise[:STDERR_LIMIT])

        if proc.returncode != 0:
            tail = noise.strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            # `python3 -m altdns` без установленного пакета даёт не FileNotFound,
            # а exit 1 и «No module named altdns» — это «не установлен»,
            # а не «доносор упал на входе», и совет у них разный.
            if "No module named" in noise and DONOR_MODULE in noise:
                return fail("altdns_not_installed",
                            "altdns не установлен: "
                            f"pip install {DONOR_DIST}=={version}")
            return fail("tool_failed",
                        f"altdns упал (exit {proc.returncode}): {last}",
                        retryable=False)

        if not os.path.exists(out_path):
            return fail("no_report",
                        "altdns не дал файл перестановок (-o): "
                        "путь, словарь или цель отвергнуты донором молча")
        try:
            lines = read_lines(out_path)
        except Exception as e:
            return fail("bad_report",
                        f"отчёт altdns не читается как UTF-8: {e}",
                        exit_code=2)

    # Дедуп + сортировка: донор уже дедуплицирует, но порядок set() не
    # детерминирован, а выход плагина должен быть воспроизводимым.
    perms = sorted(set(lines))
    return ok({"host": host, "permutations": perms,
               "total": len(perms), "generator_version": version})


if __name__ == "__main__":
    sys.exit(main())