#!/usr/bin/env python3
"""Фикстура: статус ok, но обязательного поля value нет → нарушение контракта."""
import json
import sys

json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {}}))
