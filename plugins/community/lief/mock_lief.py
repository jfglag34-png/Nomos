#!/usr/bin/env python3
"""Mock донора lief (паттерн C) для контракт-тестов — без пакета lief.

main.py запускает `<LIEF_BIN> <путь к файлу>`; мок печатает в stdout тот же
JSON, что печатает SNIPPET из main.py (с маркером @@wedra-lief@@ перед ним):
format, machine, entrypoint, imagebase, is_pie, has_nx, virtual_size,
sections[], libraries[], version.

Значения в основном режиме сняты с настоящего разбора fixtures/sample.dll
(lief 1.0.0), но сам мок НЕ проверяет, что донор ведёт себя так же, — это
делают кейсы без LIEF_BIN.

Режимы env: MOCK_SLEEP=N (wall_timeout), MOCK_UNSUPPORTED=1 (доменная ошибка
unsupported_format), MOCK_BAD_FILE=1 (формат опознан, но файл не разобран),
MOCK_NO_MODULE=1 (донор не установлен), MOCK_NO_REPORT=1 (пустой stdout),
MOCK_FAIL=1 (ненулевой код), MOCK_BAD_REPORT=1 (битый JSON), MOCK_ALT=1 (ELF
вместо PE, секций нет), MOCK_NO_VERSION=1 (ответ без версии донора),
MOCK_NO_MARK=1 (JSON без маркера — мусор в stdout от LIEF).
"""
import json
import os
import sys
import time

MARK = "@@wedra-lief@@"

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))

target = sys.argv[1] if len(sys.argv) > 1 else ""
print("lief mock: разбираю %s" % target, file=sys.stderr)

if os.environ.get("MOCK_UNSUPPORTED") == "1":
    print(json.dumps({"wedra_error": "unsupported_format",
                      "wedra_message": "файл не распознан как PE, ELF или Mach-O"}))
    sys.exit(1)
if os.environ.get("MOCK_BAD_FILE") == "1":
    print(json.dumps({"wedra_error": "bad_file",
                      "wedra_message": "LIEF не смог разобрать файл: corrupted section table"}))
    sys.exit(1)
if os.environ.get("MOCK_NO_MODULE") == "1":
    print(json.dumps({"wedra_error": "lief_not_installed",
                      "wedra_message": "пакет lief не импортируется: No module named 'lief'"}))
    sys.exit(1)
if os.environ.get("MOCK_NO_REPORT") == "1":
    sys.exit(0)
if os.environ.get("MOCK_FAIL") == "1":
    sys.stderr.write("lief mock: упал с ошибкой\n")
    sys.exit(2)
if os.environ.get("MOCK_BAD_REPORT") == "1":
    # LIEF умеет писать в stdout разбор символов, поэтому битый JSON должен
    # отличаться от «мусор + нормальный JSON» (MOCK_NO_MARK).
    sys.stdout.write("{\"format\": \"PE\", \"sections\": [,,]}\n")
    sys.exit(0)
if os.environ.get("MOCK_NO_VERSION") == "1":
    sys.stdout.write(MARK + json.dumps({
        "format": "PE", "machine": "AMD64", "entrypoint": 14782108399,
        "imagebase": 14782103552, "is_pie": True, "has_nx": True,
        "virtual_size": 86016, "sections": [], "libraries": [], "version": "",
    }))
    sys.exit(0)
if os.environ.get("MOCK_ALT") == "1":
    sys.stdout.write(MARK + json.dumps({
        "format": "ELF", "machine": "X86_64", "entrypoint": 0,
        "imagebase": 0, "is_pie": False, "has_nx": True,
        "virtual_size": 0, "sections": [], "libraries": [],
        "version": "1.0.0-d05b3499b",
    }))
    sys.exit(0)
if os.environ.get("MOCK_NO_MARK") == "1":
    # Шум LIEF перед репортом: метки нет, и разбирать stdout целиком нельзя.
    sys.stdout.write("Symbol[0] {\n  Name: atexit\n}\n")
    sys.stdout.write(json.dumps({
        "format": "PE", "machine": "AMD64", "entrypoint": 14782108399,
        "imagebase": 14782103552, "is_pie": True, "has_nx": True,
        "virtual_size": 86016,
        "sections": [{"name": ".text", "size": 7168, "memory_size": 6832,
                      "offset": 1536, "virtual_address": 4096,
                      "flags": ["CNT_CODE"]}],
        "libraries": ["KERNEL32.dll"], "version": "1.0.0-d05b3499b",
    }))
    sys.exit(0)

# Разбор fixtures/sample.dll, снятый с настоящего LIEF 1.0.0.
sys.stdout.write(MARK + json.dumps({
    "format": "PE",
    "machine": "AMD64",
    "entrypoint": 14782108399,
    "imagebase": 14782103552,
    "is_pie": True,
    "has_nx": True,
    "virtual_size": 86016,
    "sections": [
        {"name": ".text", "size": 7168, "memory_size": 6832, "offset": 1536,
         "virtual_address": 4096,
         "flags": ["CNT_CODE", "MEM_EXECUTE", "MEM_READ"]},
        {"name": ".bss", "size": 0, "memory_size": 368, "offset": 0,
         "virtual_address": 32768,
         "flags": ["CNT_UNINITIALIZED_DATA", "MEM_READ", "MEM_WRITE"]},
    ],
    "libraries": ["KERNEL32.dll", "api-ms-win-crt-runtime-l1-1-0.dll"],
    "version": "1.0.0-d05b3499b",
}))