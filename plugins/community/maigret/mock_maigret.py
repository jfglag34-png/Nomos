#!/usr/bin/env python3
"""Mock maigret CLI для контракт-тестов (без сети и без maigret).

Имитирует 0.6.x: читает аргументы как maigret (<username> [--site N]...),
пишет reports/report_<username>_simple.json в CWD. Режимы env:
MOCK_SLEEP=N — спать N секунд (тест wall_timeout);
MOCK_NO_REPORT=1 — ничего не писать (тест no_report).
"""
import json
import os
import sys
import time

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))

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

if os.environ.get("MOCK_NO_REPORT") == "1" or not username:
    sys.exit(0 if username else 1)

report = {
    "GitHub": {
        "status": {"status": "Claimed", "username": username,
                   "url": f"https://github.com/{username}"},
        "url_user": f"https://github.com/{username}",
    },
    "Wikipedia": {
        "status": {"status": "Not claimed"},
        "url_user": None,
    },
    "DeadSite": {
        "status": {"status": "Error"},
        "url_user": None,
    },
}
os.makedirs("reports", exist_ok=True)
path = f"reports/report_{username}_simple.json"
with open(path, "w", encoding="utf-8") as f:
    json.dump(report, f)
print(f"[*] JSON simple report for {username} saved in {path}", file=sys.stderr)
