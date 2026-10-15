#!/usr/bin/env python3
"""detect_secrets — поиск секретов в коде (обёртка над detect-secrets).

Вход (stdin JSON): path (файл или каталог), wall_timeout (опц., 300).

Вызов: <DETECT_SECRETS_BIN|python3 -m detect_secrets> scan <abspath> --all-files
       (cwd = корень сканирования, см. scan_cwd ниже — это обязательное условие).

Что донор реально отдаёт в stdout (проверено на detect-secrets 1.5.0):

    {"version": "1.5.0",
     "plugins_used": [...], "filters_used": [...],
     "results": {"<путь>": [{"type": ..., "filename": ..., "hashed_secret": ...,
                              "is_verified": false, "line_number": N}]},
     "generated_at": "..."}

Из этих полей наружу уходят ровно четыре, и выбор обоснован:

  version      → detector_version. Набор детекторов и есть смысл ответа: без
                 версии «находок нет» неотличимо от «эта версия не знает про
                 такой тип ключа». Пустой строкой это не подменить — версия
                 обязательна, baseline без неё считается битым.
  type         → имя находки СИМВОЛИЧЕСКОЕ, ровно как у донора («Secret Keyword»,
                 «AWS Access Key»). Свой словарь типов не заводим: донор
                 расширяет набор детекторов, и наш перечень устареет первым.
  filename     → путь ОТНОСИТЕЛЬНО корня сканирования, со слэшем независимо от
                 ОС (донор печатает разделитель родной платформы, и на Windows
                 это обратный слэш — с ним поле нельзя ни сравнивать в тесте,
                 ни сопоставлять с путём на другой машине).
  line_number  → номер строки; ключа нет, когда номер нулевой, тогда 0.

Чего в выводе НЕТ и почему:

  hashed_secret — это SHA-1 от значения секрета. Для пароля или токена низкой
                 энтропии он восстанавливается перебором за секунды, поэтому
                 наружу не отдаётся никогда (в том числе «для сверки»).
                 Поля копируются по списку ДОПУЩЕННЫХ имён, а не «всё, кроме
                 известного»: новое поле донора не должно само собой утекать в
                 вывод плагина.
  is_verified  — появляется только после `audit`/`--verify`, то есть после
                 сетевой проверки. Обязательное поле означало бы «проверено»
                 там, где никто ничего не проверял, поэтому его нет вовсе.
  generated_at — время генерации baseline: в выводе оно делает результат
                 неповторяемым, а смысла не несёт.
  plugins_used / filters_used — конфигурация донора, а не результат.

Сеть: плагин объявляет network: []. Проверено измерением — сканирование
запускается при заблокированных socket.connect/create_connection/getaddrinfo и
даёт тот же exit 0 и те же находки. Донор умеет сеть (у него есть пути
verify/audit с обращением к API ключей), но обёртка НИКОГДА не включает их:
в командной строке есть только `scan <путь> --all-files`.

Доменные ошибки: empty_path, missing_path, detect_secrets_not_installed,
timeout (retryable), no_report, tool_failed. Платформенные (exit 2): битый
JSON входа, не-объект/не-число/не-положительное wall_timeout, битый
baseline-репорт, отсутствие version в нём.
"""
import json
import os
import shutil
import subprocess
import sys

DEFAULT_WALL = 300

# stderr донора — диагностика, а не результат. Ограничение нужно, чтобы
# донос не мог вылить в stderr плагина свой простырь (ядро кладёт предел 1 МБ
# на stderr плагина, и до него донос может довести свой вывод).
STDERR_LIMIT = 8000

# Поля находки, которые разрешено копировать из baseline. Список, а не
# запрет: см. пояснение к hashed_secret в модульной документации.
FINDING_FIELDS = ("type", "filename", "line_number")


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False, exit_code=1):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return exit_code


def extract_json(text):
    """Достаёт первый JSON-объект из stdout (он может идти после прогресса).

    Возвращает (объект|None, похоже_на_json): второе нужно, чтобы отличить
    «репорта нет вовсе» (no_report) от «репорт есть, но битый» (bad_report).
    """
    try:
        obj = json.loads(text)
        if isinstance(obj, dict):
            return obj, True
    except ValueError:
        pass
    start = text.find("{")
    while start != -1:
        try:
            obj, _ = json.JSONDecoder().raw_decode(text, start)
            if isinstance(obj, dict):
                return obj, True
        except ValueError:
            pass
        start = text.find("{", start + 1)
    return None, "{" in text


def donor_command():
    """Команда запуска донора.

    По умолчанию — `python3 -m detect_secrets`, а не console-script
    `detect-secrets`: pip кладёт скрипты в каталог Scripts (на Windows), а он
    часто не в PATH, и тогда донор установлен и при этом «не найден». Модуль
    доступен ровно тогда, когда установлен пакет, то есть проверка
    «установлен ли донор» не врёт.

    DETECT_SECRETS_BIN — штатный переключатель: путь или имя из PATH. Значение
    приходит из окружения хоста, поэтому оно попадает в argv как есть и
    дополнительно не цитируется: список аргументов не даёт инъекции.
    """
    bin_env = os.environ.get("DETECT_SECRETS_BIN", "").strip()
    if not bin_env:
        return [sys.executable, "-m", "detect_secrets"]
    if "/" not in bin_env and "\\" not in bin_env:
        return [shutil.which(bin_env) or os.path.abspath(bin_env)]
    return [os.path.abspath(bin_env)]


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

    path = str(data.get("path") or "").strip()
    if not path:
        return fail("empty_path", "path пуст")

    # wall_timeout: отсутствие ключа и явный 0 — разные вещи. Наивный
    # `data.get("wall_timeout") or DEFAULT_WALL` путал их, и переданный 0 молча
    # превращался в 300: ран просил «без времени», а плагин отработал полный
    # лимит и сообщил ok. Ключа нет → дефолт; ключ есть и он неположителен →
    # ошибка входа.
    raw_wall = data.get("wall_timeout")
    if raw_wall is None:
        wall = float(DEFAULT_WALL)
    elif isinstance(raw_wall, bool):
        # JSON true/float(1) — типовый дрейф на входе, а не «секунда».
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

    # Путь проверяет обёртка, а не донор: `detect-secrets scan <несуществующий>`
    # завершается с кодом 0 и пустым results, то есть опечатка в пути выглядит
    # как «секретов не нашли». Молчаливый успех на несуществующем пути хуже
    # явной доменной ошибки.
    if not os.path.exists(path):
        return fail("missing_path", f"путь не найден: {path}")
    target = os.path.abspath(path)

    cmd = donor_command()
    if len(cmd) == 1 and cmd[0].lower().endswith(".py"):
        # .py-мок запускаем через интерпретатор — прямой exec непереносим
        # (shebang+CRLF на Linux, ассоциации на Windows).
        cmd = [sys.executable] + cmd
    # Секретов в argv нет и быть не может: единственный аргумент — путь.
    cmd += ["scan", target, "--all-files"]

    # cwd ОБЯВАН быть РАЗРЕШЁННЫМ (realpath) каталогом цели. detect-secrets
    # отдаёт имена файлов относительно os.getcwd(): в
    # util/path.get_relative_path_if_in_cwd путь файла приводится к
    # realpath и режется строковым срезом по длине os.getcwd()+'/', после
    # чего проверяется os.path.isfile() ОТ СРЕЗА (то есть относительно cwd).
    # Две несовместимые формы пути дают разную длину — 8.3-короткое имя на
    # Windows (C:\Users\X~1\... против C:\Users\Кириллица\...) или симлинк —
    # и тогда срез уходит мимо цели: файлы молча отбрасываются, на stdout
    # приходит results: {}, то есть «секретов не нашли» при полном молчании.
    # С реальным cwd обе стороны приведены к одной форме, и срез сходится.
    scan_cwd = os.path.realpath(
        target if os.path.isdir(target) else os.path.dirname(target))

    try:
        proc = subprocess.run(cmd, capture_output=True, text=True,
                              encoding="utf-8", errors="replace",
                              cwd=scan_cwd, timeout=wall)
    except FileNotFoundError:
        return fail("detect_secrets_not_installed",
                    "detect-secrets не найден: pip install detect-secrets==1.5.0 "
                    "(или укажите DETECT_SECRETS_BIN)")
    except subprocess.TimeoutExpired:
        return fail("timeout",
                    f"detect-secrets не уложился в {wall:.0f}s: уменьшите "
                    "объём сканируемого пути или увеличьте wall_timeout",
                    retryable=True)
    if proc.stderr:
        sys.stderr.write(proc.stderr[:STDERR_LIMIT])

    # Ненулевой код возврата — это отказ, даже если stdout успел напечатать
    # что-то похожее на baseline: у main() донора scan всегда возвращает 0, то
    # есть непустой baseline при ненулевом коде означает частичный обрыв
    # (краш, usage-ошибка argparse с кодом 2), и такой результат нельзя
    # выдавать за «проверили и нашли вот столько».
    if proc.returncode != 0:
        tail = (proc.stderr or "").strip().splitlines()
        last = tail[-1] if tail else f"exit {proc.returncode}"
        return fail("tool_failed",
                    f"detect-secrets упал (exit {proc.returncode}): {last}",
                    retryable=False)

    baseline, looks_like_json = extract_json(proc.stdout or "")
    if baseline is None:
        if looks_like_json:
            return fail("bad_report",
                        "baseline detect-secrets не разбирается как JSON",
                        exit_code=2)
        return fail("no_report",
                    "detect-secrets не дал baseline-JSON в stdout "
                    "(нет репорта или он пуст)")

    results = baseline.get("results")
    if results is None:
        return fail("bad_report",
                    "в baseline detect-secrets нет поля results", exit_code=2)
    if not isinstance(results, dict):
        return fail("bad_report", "results в baseline — не объект",
                    exit_code=2)

    version = baseline.get("version")
    if not isinstance(version, str) or not version.strip():
        # Версия детекторов — часть ответа, а не украшение: пустое поле здесь
        # означало бы, что находки приписаны неизвестно какой версии.
        return fail("bad_report",
                    "в baseline detect-secrets нет строкового поля version",
                    exit_code=2)

    secrets = []
    for filename, items in results.items():
        if not isinstance(items, list):
            continue
        for item in items:
            if not isinstance(item, dict):
                continue
            finding = {}
            for field in FINDING_FIELDS:
                if field == "line_number":
                    # донор опускает ключ, когда номер нулевой, и отдаёт int
                    try:
                        finding[field] = int(item.get("line_number") or 0)
                    except (TypeError, ValueError):
                        finding[field] = 0
                elif field == "filename":
                    # разделитель приводим к слэшу: путь одинаков на всех ОС
                    finding[field] = str(
                        item.get("filename") or filename).replace("\\", "/")
                else:
                    finding[field] = str(item.get(field) or "")
            secrets.append(finding)
    # Порядок донора зависит от обхода каталога, а обход — от ФС. Сортировка
    # делает вывод воспроизводимым, иначе один и тот же вход давал бы разные
    # массивы на разных машинах.
    secrets.sort(key=lambda s: (s["filename"], s["line_number"], s["type"]))

    return ok({
        "path": target,
        "secrets": secrets,
        "count": len(secrets),
        "detector_version": version.strip(),
    })


if __name__ == "__main__":
    sys.exit(main())