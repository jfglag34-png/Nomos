#!/usr/bin/env python3
"""Фикстура fileref-тестов: путь не трогает, просто честно отвечает ok."""
import json
import sys

_ = json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"ok": True}}))
