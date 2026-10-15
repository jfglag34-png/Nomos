#!/usr/bin/env python3
"""Mock донора pefile для веток отказа (пакет pefile ставится рядом, но он не
умеет по заказу упасть на конкретном входе).

Имитирует тот же протокол, что и SNIPPET из main.py: exit 0 и один JSON-репорт
в stdout — успех; exit 1 и {"wedra_error": …, "wedra_message": …} — доменная
ошибка.

Числа в репорте — из настоящей фикстуры fixtures/sample.dll (18 секций,
6 импортируемых DLL, machine 34404), но не весь её состав: здесь важны ветки
отказа, а не точное содержимое файла. Точный состав проверяют кейсы БЕЗ
PEFILE_BIN — они запускают настоящий pefile по настоящей фикстуре.

Режимы env:
  MOCK_SLEEP=N        — зависнуть (тест wall_timeout)
  MOCK_FAIL=1         — ненулевой код выхода без маркера → tool_failed
  MOCK_NO_REPORT=1    — пустой stdout при exit 0 → no_report
  MOCK_NO_JSON=1      — stdout есть, JSON-объекта нет → no_report
  MOCK_BAD_REPORT=1   — битый JSON → bad_report
  MOCK_NOT_OBJECT=1   — валидный JSON, но не объект → bad_report
  MOCK_NO_VERSION=1   — репорт без parser_version → bad_report
  MOCK_BAD_TYPES=1    — machine_id строкой, sections не массив → bad_report
  MOCK_PROGRESS=1     — прогресс в stdout перед репортом → репорт виден
  MOCK_ORDINALS=1     — импорт по номеру: имя пустое, ordinal заполнен
  MOCK_UNKNOWN_MACH=1 — код архитектуры, которого нет в словаре донора
  MOCK_NO_MODULE=1    — донор не импортируется → pefile_not_installed
  MOCK_NOT_PE=1       — маркер not_pe
  MOCK_BAD_FILE=1     — маркер bad_file

Отдельный режим MOCK_FILE=<строка> кладёт в репорт поле file. Обёртка его
игнорирует: путь в выводе плагина задаёт оператор, а не донор.
"""
import json
import os
import sys
import time

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))
if os.environ.get("MOCK_FAIL") == "1":
    print("[*] Mock: падаю с ненулевым кодом", file=sys.stderr)
    sys.exit(3)

if os.environ.get("MOCK_NO_MODULE") == "1":
    print(json.dumps({"wedra_error": "pefile_not_installed",
                      "wedra_message": "пакет pefile не импортируется"}))
    sys.exit(1)
if os.environ.get("MOCK_NOT_PE") == "1":
    print(json.dumps({"wedra_error": "not_pe",
                      "wedra_message": "файл не читается как PE: DOS Header "
                                       "magic not found."}))
    sys.exit(1)
if os.environ.get("MOCK_BAD_FILE") == "1":
    print(json.dumps({"wedra_error": "bad_file",
                      "wedra_message": "не удалось разобрать PE: OSError(13)"}))
    sys.exit(1)

if os.environ.get("MOCK_NO_REPORT") == "1":
    sys.exit(0)
if os.environ.get("MOCK_NO_JSON") == "1":
    print("[*] Mock: смотреть надо в stdout, а тут только шум")
    sys.exit(0)
if os.environ.get("MOCK_BAD_REPORT") == "1":
    print('{"parser_version": "2024.8.26", "sections": [oops')
    sys.exit(0)
if os.environ.get("MOCK_NOT_OBJECT") == "1":
    print(json.dumps([1, 2, 3]))
    sys.exit(0)
if os.environ.get("MOCK_PROGRESS") == "1":
    print("[*] Mock: читаю PE ...")
    print("[*] Mock: каталог импортов разобран")

if os.environ.get("MOCK_ORDINALS") == "1":
    imports = [{"dll": "USER32.dll",
                "symbols": [{"name": "", "ordinal": 17},
                            {"name": "MessageBoxA", "ordinal": None}]}]
    sections = [{"name": ".text", "virtual_address": 4096, "size": 512,
                 "characteristics": 1610612768,
                 "flags": ["IMAGE_SCN_CNT_CODE", "IMAGE_SCN_MEM_EXECUTE",
                           "IMAGE_SCN_MEM_READ"]}]
else:
    imports = [
        {"dll": "KERNEL32.dll",
         "symbols": [{"name": "GetLastError", "ordinal": None},
                     {"name": "GetModuleHandleA", "ordinal": None}]},
        {"dll": "api-ms-win-crt-string-l1-1-0.dll",
         "symbols": [{"name": "strlen", "ordinal": None}]},
    ]
    sections = [
        {"name": ".text", "virtual_address": 4096, "size": 7168,
         "characteristics": 1610612768,
         "flags": ["IMAGE_SCN_CNT_CODE", "IMAGE_SCN_MEM_EXECUTE",
                   "IMAGE_SCN_MEM_READ"]},
        {"name": "/4", "virtual_address": 20480, "size": 512,
         "characteristics": 3221225536, "flags": []},
    ]

report = {
    "parser_version": "2024.8.26",
    "machine_id": 0x1234 if os.environ.get("MOCK_UNKNOWN_MACH") == "1" else 34404,
    "machine": "" if os.environ.get("MOCK_UNKNOWN_MACH") == "1"
               else "IMAGE_FILE_MACHINE_AMD64",
    "sections": sections,
    "imports": imports,
    # Поля, которых обёртка не имеет права тащить в вывод. MOCK_FILE добавляет
    # ещё и чужой путь: путь в выводе плагина задаёт оператор.
    "pdb_path": r"C:\build\agent\release\obfuscated.dll.pdb",
    "imphash": "50f3e904f15caab47bf72f2c742ac4ae",
    "timestamp": 1750000000,
}
if os.environ.get("MOCK_FILE"):
    report["file"] = os.environ["MOCK_FILE"]
if os.environ.get("MOCK_NO_VERSION") == "1":
    del report["parser_version"]
if os.environ.get("MOCK_BAD_TYPES") == "1":
    report["machine_id"] = "не число"
    report["sections"] = "не массив"

print(json.dumps(report, ensure_ascii=False))
print("[*] Mock: готово", file=sys.stderr)