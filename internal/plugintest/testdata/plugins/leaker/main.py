#!/usr/bin/env python3
"""Фикстура: возвращает незадекларированное поле junk → enforce отбрасывает."""
import json
import sys

json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"value": "x", "junk": "undeclared"}}))
