#!/usr/bin/env python3
"""csv_column — достаёт одну колонку из rows в плоский массив строк.

Мост между загрузчиками таблиц и батч-плагинами: csv_loader отдаёт массив
объектов (по строке таблицы), а batch_email_triage и phone_normalizer ждут
плоский массив значений. Без сети, без файлов, без зависимостей.

Протокол (PROTOCOL.md):
  stdin  ← JSON {"rows": [{"email": "a@b"}, ...], "column": "email"}
  stdout → status: ok/error
"""
import json
import sys

try:
    sys.stdin.reconfigure(encoding="utf-8")
    sys.stdout.reconfigure(encoding="utf-8")
except Exception:
    pass


def ok(output):
    print(json.dumps({"status": "ok", "output": output}, ensure_ascii=False))
    return 0


def fail(code, message, retryable=False):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": retryable}},
                     ensure_ascii=False))
    return 1


def fail_platform(code, message):
    print(json.dumps({"status": "error",
                      "error": {"code": code, "message": message,
                                "retryable": False}},
                     ensure_ascii=False))
    return 2


def cell_to_text(value):
    """Ячейка → строка. None и пустая строка считаются отсутствующим значением."""
    if value is None:
        return ""
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (int, float)):
        return str(value)
    if isinstance(value, str):
        return value.strip()
    # вложенный объект/массив в ячейке — это не значение колонки
    return ""


def main():
    try:
        data = json.load(sys.stdin)
    except Exception as e:
        return fail_platform("bad_input", f"невалидный JSON: {e}")

    if not isinstance(data, dict):
        return fail_platform("bad_input", "stdin должен быть JSON-объектом")

    rows = data.get("rows")
    if not isinstance(rows, list):
        return fail_platform(
            "bad_input",
            f"rows должен быть массивом, пришло {type(rows).__name__}")

    column = data.get("column")
    if not isinstance(column, str):
        return fail_platform(
            "bad_input",
            f"column должен быть строкой, пришло {type(column).__name__}")
    column = column.strip()
    if not column:
        return fail("empty_column", "поле column пустое")

    if not rows:
        return fail("empty_rows", "массив rows пуст — не из чего выбирать колонку")

    if not all(isinstance(r, dict) for r in rows):
        # csv_loader с has_header: false отдаёт массивы, а не объекты
        return fail("not_objects",
                    "rows содержит не объекты: для has_header: false у колонки нет имени")

    available = []
    for r in rows:
        for key in r:
            if key not in available:
                available.append(key)
    if column not in available:
        return fail("no_column",
                    f"колонки {column!r} нет; в таблице: {', '.join(available) or '—'}")

    values = []
    missing = 0
    seen = set()
    duplicates = 0
    for r in rows:
        text = cell_to_text(r.get(column))
        if not text:
            missing += 1
            continue
        values.append(text)
        key = text.lower()
        if key in seen:
            duplicates += 1
        else:
            seen.add(key)

    return ok({
        "values": values,
        "count": len(values),
        "missing": missing,
        "duplicates": duplicates,
        "unique": len(seen),
    })


if __name__ == "__main__":
    sys.exit(main())
