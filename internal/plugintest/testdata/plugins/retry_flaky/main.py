#!/usr/bin/env python3
# Фикстура 'retry': доменная retryable-ошибка на первые 2 попытки, на 3-й — ok.
#
# Счётчик попыток лежит ВНЕ каталога плагина (путь берётся из RETRY_COUNTER).
# Раньше он писался рядом с main.py, и это перестало работать после инверсии
# доверия (H1): доверенность определяется хэшем содержимого каталога, поэтому
# первая же запись _counter меняла хэш и отзывала доверие у плагина, который
# только что отработал. Само по себе это верное поведение — плагин, пишущий в
# собственный каталог, сам себя модифицирует, и ровно поэтому песочница делает
# каталог плагина read-only. Но для ФИКСТУРЫ это просто неудачный способ хранить
# состояние, поэтому счётчик вынесен наружу.
import json
import os
import sys

counter_path = os.environ.get("RETRY_COUNTER") or os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "_counter")

try:
    with open(counter_path) as f:
        n = int(f.read().strip())
except Exception:
    n = 0
n += 1
with open(counter_path, "w") as f:
    f.write(str(n))

if n <= 2:
    print(json.dumps({"status": "error", "error": {
        "code": "rate_limit", "message": "429 slow down", "retryable": True}}))
    sys.exit(1)

print(json.dumps({"status": "ok", "output": {"result": "ok"}}))
