#!/usr/bin/env python3
"""Фикстура: доменные ошибки.

- value == "bad"   → exit 1, retryable=false
- value == "flaky" → exit 1, retryable=true (имитация rate-limit)
- иначе            → ok
"""
import json
import sys

data = json.load(sys.stdin)
v = data.get("value")

if v == "bad":
    print(json.dumps({"status": "error", "error": {
        "code": "bad_value", "message": "отрицательный результат", "retryable": False}}))
    sys.exit(1)
if v == "flaky":
    print(json.dumps({"status": "error", "error": {
        "code": "rate_limit", "message": "429 slow down", "retryable": True}}))
    sys.exit(1)

print(json.dumps({"status": "ok", "output": {"result": v}}))
