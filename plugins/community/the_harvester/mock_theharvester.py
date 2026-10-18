#!/usr/bin/env python3
"""Mock theHarvester CLI для контракт-тестов (без сети и без theHarvester).

Имитирует 4.9.x: читает -d <domain> / -f <file> / -b <sources> / -l <n>,
пишет <file>.json в CWD. Режимы env: MOCK_SLEEP=N (тест wall_timeout),
MOCK_NO_REPORT=1 (тест no_report), MOCK_EMPTY=1 (набор данных пуст).
"""
import json
import os
import sys
import time

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))

args = sys.argv[1:]
domain = None
out_file = None
i = 0
while i < len(args):
    if args[i] == "-d":
        domain = args[i + 1]
        i += 2
    elif args[i] == "-f":
        out_file = args[i + 1]
        i += 2
    elif args[i] in ("-b", "-l"):
        i += 2
    elif args[i].startswith("-"):
        i += 1
    else:
        i += 1

if not domain or not out_file:
    sys.exit(1)

if os.environ.get("MOCK_NO_REPORT") == "1":
    sys.exit(0)

if os.environ.get("MOCK_EMPTY") == "1":
    report = {"cmd": "mock", "hosts": [], "shodan": []}
else:
    report = {
        "cmd": "mock",
        "emails": ["a@example.com", "b@example.com"],
        "hosts": ["api.example.com:1.2.3.4"],
        "people": ["John Doe"],
        "vhosts": ["v1.example.com:1.2.3.4"],
        "asns": ["AS15169"],
        "interesting_urls": ["https://example.com/admin"],
        "shodan": [],
    }
with open(out_file + ".json", "w", encoding="utf-8") as f:
    json.dump(report, f)
print("[*] JSON File saved.", file=sys.stderr)
