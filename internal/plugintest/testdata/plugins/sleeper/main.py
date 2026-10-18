#!/usr/bin/env python3
"""Фикстура: спит 5 секунд → для теста таймаута шага."""
import json
import sys
import time

json.load(sys.stdin)
time.sleep(5)
print(json.dumps({"status": "ok", "output": {"result": "slept"}}))
