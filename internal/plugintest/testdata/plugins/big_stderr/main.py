#!/usr/bin/env python3
"""Conformance: 2MB в stderr — ран жив, stderr обрезан (лимит 1MB)."""
import sys
import json

sys.stderr.write("E" * (2 * 1024 * 1024))
sys.stderr.flush()
json.load(sys.stdin)
json.dump({"status": "ok", "output": {"done": True}}, sys.stdout)
