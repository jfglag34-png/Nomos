#!/usr/bin/env python3
"""Mock донора pyelftools (паттерн C) для контракт-тестов — без пакета elftools.

main.py запускает `<PYELFTOOLS_BIN> <путь к файлу>`; мок печатает в stdout тот
же JSON, что печатает SNIPPET из main.py: class, machine, type, entry,
sections[], imported_libraries[] и version. Значения взяты с фикстуры
libfixture.so (реальный разбор, снят с донора), но сам мок НЕ проверяет, что
донор ведёт себя так же, — это делают кейсы без PYELFTOOLS_BIN. Режимы env: MOCK_SLEEP=N (wall_timeout), MOCK_NOT_ELF=1
(доменная ошибка not_elf), MOCK_BAD_FILE=1 (bad_file), MOCK_NO_MODULE=1 (донор
не установлен), MOCK_NO_REPORT=1 (пустой stdout), MOCK_FAIL=1 (ненулевой код),
MOCK_BAD_REPORT=1 (битый JSON), MOCK_ALT=1 (ELF32, секций нет, библиотек нет).
"""
import json
import os
import sys
import time

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))

target = sys.argv[1] if len(sys.argv) > 1 else ""
print("pyelftools mock: разбираю %s" % target, file=sys.stderr)

if os.environ.get("MOCK_NOT_ELF") == "1":
    print(json.dumps({"wedra_error": "not_elf",
                      "wedra_message": "файл не читается как ELF: Invalid ELF magic"}))
    sys.exit(1)
if os.environ.get("MOCK_BAD_FILE") == "1":
    print(json.dumps({"wedra_error": "bad_file",
                      "wedra_message": "не удалось разобрать ELF: truncated section header"}))
    sys.exit(1)
if os.environ.get("MOCK_NO_MODULE") == "1":
    print(json.dumps({"wedra_error": "pyelftools_not_installed",
                      "wedra_message": "пакет pyelftools не импортируется: No module named 'elftools'"}))
    sys.exit(1)
if os.environ.get("MOCK_NO_REPORT") == "1":
    sys.exit(0)
if os.environ.get("MOCK_FAIL") == "1":
    sys.stderr.write("pyelftools mock: упал с ошибкой\n")
    sys.exit(2)
if os.environ.get("MOCK_BAD_REPORT") == "1":
    sys.stdout.write("{\"class\": \"ELF64\", \"sections\": [,,]}\n")
    sys.exit(0)

if os.environ.get("MOCK_ALT") == "1":
    # ELF32 ET_REL без секций: донор так не отдаёт, это проверка ветки
    # «секций нет — count 0, libraries []».
    print(json.dumps({"class": "ELF32", "machine": "EM_386", "type": "ET_REL",
                      "entry": 0, "sections": [],
                      "imported_libraries": [], "version": "0.33"}))
    sys.exit(0)

if os.environ.get("MOCK_NO_VERSION") == "1":
    # Ответ без версии: плагин не должен приписать разбор неизвестной версии.
    print(json.dumps({"class": "ELF64", "machine": "EM_X86_64",
                      "type": "ET_DYN", "entry": 448, "sections": [],
                      "imported_libraries": [], "version": ""}))
    sys.exit(0)

# Разбор fixtures/libfixture.so, снятый с настоящего pyelftools 0.33.
print(json.dumps({
    "class": "ELF64",
    "machine": "EM_X86_64",
    "type": "ET_DYN",
    "entry": 448,
    "sections": [
        {"name": "", "type": "SHT_NULL", "flags": 0, "size": 0, "addr": 0},
        {"name": ".dynstr", "type": "SHT_STRTAB", "flags": 2, "size": 81,
         "addr": 288},
        {"name": ".dynsym", "type": "SHT_DYNSYM", "flags": 2, "size": 72,
         "addr": 376},
        {"name": ".text", "type": "SHT_PROGBITS", "flags": 6, "size": 54,
         "addr": 448},
        {"name": ".dynamic", "type": "SHT_DYNAMIC", "flags": 3, "size": 144,
         "addr": 504},
        {"name": ".data", "type": "SHT_PROGBITS", "flags": 3, "size": 16,
         "addr": 648},
        {"name": ".bss", "type": "SHT_NOBITS", "flags": 3, "size": 64,
         "addr": 672},
        {"name": ".shstrtab", "type": "SHT_STRTAB", "flags": 0, "size": 53,
         "addr": 0},
    ],
    "imported_libraries": ["libc.so.6", "libpthread.so.0"],
    "version": "0.33",
}))