#!/usr/bin/env python3
"""Фикстура: optional-потребитель. Если продюсер был skip'нут — вход отсутствует."""
import json
import sys

data = json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"got": data.get("data", "<none>")}}))
