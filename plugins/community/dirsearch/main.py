#!/usr/bin/env python3
"""dirsearch — брутфорс директорий и файлов (обёртка над CLI dirsearch).

Вход (stdin JSON): url, extensions (опц.), wordlist (опц.), threads (опц., 25),
wall_timeout (опц., общий лимит, 600).

Вызов: <DIRSEARCH_BIN|dirsearch> -u <url> -o <tmp/report.json>
       --output-formats json -q --no-color --disable-cli --max-time N
       [-e EXT] [-w LIST] [-t N]   (cwd = временная папка, stdin = /dev/null).

Флаги сверены с docs/options.md (maurosoria/dirsearch):
  -u/--url, -e/--extensions, -w/--wordlists, -t/--threads, -o/--output-file,
  -O/--output-formats (simple|plain|json|xml|md|csv|html|sqlite) — ВНИМАНИЕ:
  -O это формат, а -o это файл; -q/--quiet-mode, --no-color, --disable-cli,
  --max-time=SECONDS. Рекурсии -r в команде нет: она выключена по умолчанию и
  включать её без просьбы пользователя нельзя. Флага -r «выключить» не
  существует — лишний -r просто включил бы рекурсию.

Интерактив в dirsearch один и он по SIGINT (handle_pause → input()): с
закрытым stdin он даёт EOF, а не висит. --exit-on-error намеренно НЕ
передаём: он завершает dirsearch кодом 0 на первой сетевой ошибке, и
недописанный отчёт был бы выдан за полный.

Отчёт: JSONReport пишет {"info": {...}, "results": [{url, status,
contentLength, contentType, redirect}]} и создаётся сразу при старте, так что
пустой results — честный «ничего не нашлось», а не ошибка. Старые версии
писали content-length/content_length — принимаем оба.

Выход (stdout JSON): {url, found[{path,status,size}], count}; found отсортированы
по path и дедуплицированы.
Доменные ошибки: empty_url, bad_url, missing_wordlist, bad_threads,
dirsearch_not_installed, timeout (retryable), tool_failed, no_report.
Платформенные (exit 2): битый JSON входа, нечитаемый отчёт (bad_report).
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit

try:
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass

DEFAULT_THREADS = 25
DEFAULT_WALL = 600
SIZE_KEYS = ("contentLength", "content-length", "content_length", "length")


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


def as_int(value, default=0):
    try:
        return int(float(value))
    except (TypeError, ValueError):
        return default


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail("bad_input", f"невалидный JSON на входе: {e}", exit_code=2)
    if not isinstance(data, dict):
        return fail("bad_input", "ожидается JSON-объект на входе", exit_code=2)

    url = str(data.get("url") or "").strip()
    if not url:
        return fail("empty_url", "url пуст")
    if "://" not in url:
        url = "https://" + url
    if not url.startswith(("http://", "https://")):
        return fail("bad_url", f"схема не http(s): {url}")

    extensions = ",".join(str(data.get("extensions") or "").split())
    wordlist = str(data.get("wordlist") or "").strip()
    if wordlist and not os.path.isfile(wordlist):
        return fail("missing_wordlist", f"файл словаря не найден: {wordlist}")
    if wordlist:
        # Относительный путь проверяется от каталога плагина (cwd процесса), а
        # dirsearch поедет с cwd во временную папку: отдавать ему относительный
        # путь — значит получить «words.txt does not exist» на живом доноре.
        # Абсолютным путём файл проверяется и передаётся одинаково.
        wordlist = os.path.abspath(wordlist)

    raw_threads = data.get("threads")
    if raw_threads is None or raw_threads == "":
        threads = DEFAULT_THREADS
    else:
        try:
            threads = int(float(raw_threads))
        except (TypeError, ValueError):
            return fail("bad_threads", "threads обязан быть целым")
        if threads < 1:
            return fail("bad_threads", "threads должен быть >= 1")

    try:
        wall = float(data.get("wall_timeout") or DEFAULT_WALL)
    except (TypeError, ValueError):
        return fail("bad_wall_timeout", "wall_timeout обязан быть числом",
                    exit_code=2)

    bin_env = os.environ.get("DIRSEARCH_BIN", "dirsearch").strip()
    # subprocess поедет с cwd во временную папку: имя из PATH ищем which'ем,
    # путь — приводим к абсолютному
    if "/" not in bin_env and "\\" not in bin_env:
        bin_env = shutil.which(bin_env) or os.path.abspath(bin_env)
    else:
        bin_env = os.path.abspath(bin_env)
    if bin_env.lower().endswith(".py"):
        # v0.29: .py-мок запускаем через интерпретатор (см. holehe)
        cmd = [sys.executable, bin_env]
    else:
        cmd = [bin_env]

    with tempfile.TemporaryDirectory() as td:
        report_path = os.path.join(td, "report.json")
        cmd += ["-u", url, "-o", report_path, "--output-formats", "json",
                "-q", "--no-color", "--disable-cli",
                "--max-time", str(max(1, int(wall)))]
        if extensions:
            cmd += ["-e", extensions]
        if wordlist:
            cmd += ["-w", wordlist]
        cmd += ["-t", str(threads)]

        try:
            proc = subprocess.run(cmd, capture_output=True, text=True,
                                  cwd=td, timeout=wall,
                                  stdin=subprocess.DEVNULL)
        except FileNotFoundError:
            return fail("dirsearch_not_installed",
                        "dirsearch не найден: pip install dirsearch "
                        "(или укажите DIRSEARCH_BIN)")
        except subprocess.TimeoutExpired:
            return fail("timeout",
                        f"dirsearch не уложился в {wall:.0f}s: уменьшите "
                        "словарь/threads или увеличьте wall_timeout",
                        retryable=True)
        if proc.stderr:
            sys.stderr.write(proc.stderr)

        if proc.returncode != 0:
            tail = (proc.stderr or proc.stdout or "").strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            return fail("tool_failed",
                        f"dirsearch упал (exit {proc.returncode}): {last}")

        if not os.path.isfile(report_path):
            tail = (proc.stderr or proc.stdout or "").strip().splitlines()
            last = tail[-1] if tail else f"exit {proc.returncode}"
            return fail("no_report", f"dirsearch не дал JSON-отчёт: {last}")
        try:
            with open(report_path, encoding="utf-8") as f:
                report = json.load(f)
        except Exception as e:
            return fail("bad_report", f"не прочитан JSON dirsearch: {e}",
                        exit_code=2)

    if not isinstance(report, dict) or not isinstance(report.get("results"),
                                                      list):
        return fail("bad_report",
                    "в отчёте dirsearch нет списка results", exit_code=2)

    found = []
    seen = set()
    for item in report["results"]:
        if not isinstance(item, dict):
            continue
        item_url = str(item.get("url") or "").strip()
        path = str(item.get("path") or "").strip()
        if not path and item_url:
            path = urlsplit(item_url).path or "/"
        if not path:
            continue
        if path in seen:
            continue
        seen.add(path)
        size = 0
        for key in SIZE_KEYS:
            if key in item:
                size = as_int(item.get(key))
                break
        found.append({"path": path,
                      "status": as_int(item.get("status")),
                      "size": size})

    found.sort(key=lambda item: item["path"])
    return ok({"url": url, "found": found, "count": len(found)})


if __name__ == "__main__":
    sys.exit(main())