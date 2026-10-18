#!/usr/bin/env python3
"""Mock maigret CLI (пустой репорт): имитирует «ник не найден нигде».

Пишет reports/report_<username>_simple.json со пустым объектом — как
реальный maigret 0.6.x при нуле совпадений.
"""
import json
import os
import sys

VALUE_FLAGS = {"--site", "--timeout", "-J", "-n", "--retries", "-fo", "-p", "-T"}
args = sys.argv[1:]
username = None
i = 0
while i < len(args):
    if args[i] in VALUE_FLAGS:
        i += 2
    elif args[i].startswith("-"):
        i += 1
    elif username is None:
        username = args[i]
        i += 1
    else:
        i += 1

if not username:
    sys.exit(1)

os.makedirs("reports", exist_ok=True)
path = f"reports/report_{username}_simple.json"
with open(path, "w", encoding="utf-8") as f:
    json.dump({}, f)
print(f"[*] JSON simple report for {username} saved in {path}", file=sys.stderr)
