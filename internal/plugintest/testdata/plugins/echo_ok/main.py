#!/usr/bin/env python3
"""Фикстура: эхо входа. Используется в happy-path тестах."""
import json
import sys

data = json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"value": data.get("value")}}))
