#!/usr/bin/env python3
"""Mock holehe CLI для контракт-тестов (без сети и без пакета holehe).

Имитирует 1.6x: читает <target> (после флагов), пишет
holehe_mock_<target>_results.csv в CWD. Режимы env: MOCK_SLEEP=N (тест
wall_timeout), MOCK_NO_CSV=1 (тест no_report).
"""
import os
import sys
import time

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))

args = [a for a in sys.argv[1:] if not a.startswith("-")]
if args and not args[0].lstrip("-").isdigit():
    target = args[0]
else:
    target = None
# -T N: число после -T не попадёт в args[0] только если target перед ним
# порядок аргументов в main.py: [bin, target, -C, --no-color, -T, N]
if os.environ.get("MOCK_NO_CSV") == "1" or not target:
    sys.exit(0 if target else 1)

rows = [
    "name,domain,method,frequent_rate_limit,rateLimit,exists,emailrecovery,phoneNumber,others",
    "github,github.com,login,False,False,True,,,",
    "mail_ru,mail.ru,register,False,True,False,,,",
    "icq,icq.com,login,False,False,False,,+79991234567,",
]
with open(f"holehe_mock_{target}_results.csv", "w", encoding="utf-8") as f:
    f.write("\n".join(rows) + "\n")
print(f"[+] All results exported to holehe_mock_{target}_results.csv",
      file=sys.stderr)
