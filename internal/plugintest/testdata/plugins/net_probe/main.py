#!/usr/bin/env python3
"""Фикстура: отдаёт значение WEDRA_NETWORK из env (контракт declare-now)."""
import json
import os
import sys

data = json.load(sys.stdin)
print(json.dumps({"status": "ok", "output": {"value": os.environ.get("WEDRA_NETWORK", "unset")}}))
