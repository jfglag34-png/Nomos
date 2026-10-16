#!/usr/bin/env python3
"""Mock altdns CLI для контракт-тестов (без сети и без пакета altdns).

Имитирует altdns 1.0.2: читает -i <файл субдоменов>, -w <словарь> и пишет
-о <файл> — по одному перестановочному имени в строке (dev.www, www.dev,
www-dev, dev-www, wwwdev, devwww; с -n ещё и www-0..www-9). Ни одного
DNS-запроса. Строки пишутся в обратном порядке и с дублями — дедупликацию и
сортировку обязан сделать вызывающий код, как это делает настоящий altdns
(remove_duplicates через set()).

Моделирует ровно то, что делает настоящий донор на этом входе (сверено с
py-altdns 1.0.2: 12 перестановок для www.example.com со словарём [dev, test],
6 для [dev], 26 с -n, 1 для example.com), и НЕ проверяет, что донор ведёт себя
так же — это делают кейсы без ALTDNS_BIN.

Режимы env: MOCK_SLEEP=N (тест wall_timeout), MOCK_NO_REPORT=1 (нет файла -o),
MOCK_FAIL=1 (ненулевой код выхода), MOCK_BAD_REPORT=1 (отчёт не в UTF-8),
MOCK_EMPTY=1 (пустой файл -o), MOCK_MISSING_MODULE=1 (тот самый exit 1 и
«No module named altdns», который даёт `python3 -m altdns` без пакета).
"""
import os
import sys
import time


def opt(argv, flag):
    for i, arg in enumerate(argv):
        if arg == flag and i + 1 < len(argv):
            return argv[i + 1]
        if arg.startswith(flag + "="):
            return arg.split("=", 1)[1]
    return None


if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))

if os.environ.get("MOCK_MISSING_MODULE") == "1":
    # Ровно то, что печатает `python3 -m altdns`, когда пакета нет:
    # traceback в stderr и код 1. Обёртка обязана отличить это от «доносор упал».
    print("Traceback (most recent call last):", file=sys.stderr)
    print("  ModuleNotFoundError: No module named 'altdns'", file=sys.stderr)
    sys.exit(1)

argv = sys.argv[1:]
in_path = opt(argv, "-i")
word_path = opt(argv, "-w")
out_path = opt(argv, "-o")
if not in_path or not word_path or not out_path:
    print("altdns: -i, -w и -o обязательны", file=sys.stderr)
    sys.exit(2)

if os.environ.get("MOCK_NO_REPORT") == "1":
    sys.exit(0)
if os.environ.get("MOCK_FAIL") == "1":
    print("altdns: Traceback (most recent call last): ...", file=sys.stderr)
    sys.exit(2)

with open(in_path, encoding="utf-8") as f:
    hosts = [line.strip().lower().rstrip(".") for line in f if line.strip()]
with open(word_path, encoding="utf-8") as f:
    words = [line.strip().lower() for line in f if line.strip()]

number_suffix = "-n" in argv

if os.environ.get("MOCK_EMPTY") == "1":
    with open(out_path, "w", encoding="utf-8") as f:
        f.write("")
    print("[*] Completed in 0:00:00", file=sys.stderr)
    sys.exit(0)

generated = []
for host in hosts:
    parts = host.split(".")
    if len(parts) >= 3:
        labels = parts[:-2]
        tail = ".".join(parts[-2:])
    else:
        labels = parts[:1]
        tail = ".".join(parts[1:]) or host
    for word in words:
        for index in range(len(labels)):
            rest = ".".join(labels[:index] + labels[index + 1:])
            prefix = (rest + ".") if rest else ""
            generated.append(word + "." + prefix + labels[index] + "." + tail)
            generated.append(prefix + labels[index] + "." + word + "." + tail)
            generated.append(prefix + labels[index] + "-" + word + "." + tail)
            generated.append(word + "-" + prefix + labels[index] + "." + tail)
            generated.append(prefix + labels[index] + word + "." + tail)
            generated.append(word + prefix + labels[index] + "." + tail)
            if number_suffix:
                for num in range(10):
                    generated.append(prefix + labels[index] + "-" + str(num)
                                     + "." + tail)
                    generated.append(prefix + labels[index] + str(num)
                                     + "." + tail)

deduped = list(dict.fromkeys(generated))
deduped.reverse()

with open(out_path, "wb") as f:
    if os.environ.get("MOCK_BAD_REPORT") == "1":
        f.write(b"\xff\xfe\x00 not utf-8\n")
    else:
        f.write(("\n".join(deduped) + "\n").encode("utf-8"))

print("[*] Wrote %d altered subdomains to %s" % (len(deduped), out_path),
      file=sys.stderr)