#!/usr/bin/env python3
"""Фикстура: принимает и возвращает число (для контракт-тестов входа)."""
import json
import sys

data = json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"n": data.get("n")}}))
