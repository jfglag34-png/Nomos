#!/usr/bin/env python3
"""Фикстура: манифест обещает string, плагин возвращает number.
Контракт-тест обязан поймать дрейф типа."""
import json
import sys

json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"value": 42}}))
