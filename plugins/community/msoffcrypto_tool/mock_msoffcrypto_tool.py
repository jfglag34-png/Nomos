#!/usr/bin/env python3
"""Mock донор msoffcrypto-tool для веток отказа (паттерн C).

Обёртка зовёт донора одним и тем же способом — `python3 -c <сниппет>`, который
печатает в stdout РОВНО ОДИН JSON, — поэтому мок повторяет протокол сниппета, а
не CLI: тот же argv (путь к файлу и путь для результата, второй может быть
пустым), тот же stdin с JSON {"password": ...}, тот же маркер доменной ошибки
{"wedra_error": ..., "wedra_message": ...} и exit 1.

Где применяется: только кейсы, которые настоящим донором не вызвать — упасть
на конкретном входе, зависнуть или отдать битый ответ он не может. Контракт
(порты, типы и значения) проверяют кейсы без MSOFFCRYPTO_TOOL_BIN: они
запускают настоящий msoffcrypto-tool по фикстурам из fixtures/.

Значения в успешном ответе — те же, что отдаёт настоящий донор на фикстурах
(проверено на msoffcrypto-tool 6.0.0): шифрование agile, tool_version 6.0.0,
а размер открытого текста 9707 — это ровно размер fixtures/plain.docx.

Режимы env:
  MOCK_SLEEP=N            — зависнуть на N секунд (ветка таймаута);
  MOCK_FAIL=1             — упасть с кодом 2 без маркера (ветка tool_failed);
  MOCK_NO_REPORT=1        — не печатать ничего (ветка no_report);
  MOCK_BAD_REPORT=1       — напечатать не-JSON (ветка bad_report);
  MOCK_NO_VERSION=1       — ответ без поля version (ветка version_unknown);
  MOCK_NOT_ENCRYPTED=1    — документ не зашифрован, а out задан;
  MOCK_BAD_PASSWORD=1     — пароль не подошёл;
  MOCK_UNSUPPORTED=1      — документ не офисный;
  MOCK_PLAIN=1            — документ есть, но не зашифрован (тип "plain");
  MOCK_EXTRA_FIELDS=1     — добавить в ответ office.info-подобные поля:
                            обёртка обязана отрезать их молча.

Пароль из stdin в stdout не печатается: в выводе плагина его быть не должно.
"""
import json
import os
import sys
import time

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))
if os.environ.get("MOCK_FAIL") == "1":
    print("msoffcrypto-tool: внутренняя ошибка", file=sys.stderr)
    sys.exit(2)

# Тот же разбор stdin, что у боевого сниппета: пароль приходит сюда, а не в argv.
try:
    request = json.load(sys.stdin)
    password = str(request.get("password") or "") if isinstance(request, dict) else ""
except Exception:
    password = ""

out_path = sys.argv[2] if len(sys.argv) > 2 else ""


def bail(code, message):
    sys.stdout.write(json.dumps({"wedra_error": code,
                                 "wedra_message": message}))
    sys.exit(1)


if os.environ.get("MOCK_UNSUPPORTED") == "1":
    bail("unsupported_format", "Unsupported file format")
if os.environ.get("MOCK_NOT_ENCRYPTED") == "1" and out_path:
    bail("not_encrypted", "документ не зашифрован: расшифровывать нечего")
if os.environ.get("MOCK_BAD_PASSWORD") == "1" and out_path:
    bail("bad_password", "The file could not be decrypted with this password")

if os.environ.get("MOCK_NO_REPORT") == "1":
    sys.exit(0)
if os.environ.get("MOCK_BAD_REPORT") == "1":
    sys.stdout.write("{это не JSON")
    sys.exit(0)

plain = os.environ.get("MOCK_PLAIN") == "1"
report = {"encrypted": not plain,
          "encryption_type": "plain" if plain else "agile",
          "decrypted": bool(out_path) and not plain,
          "decrypted_size": 0,
          "version": "6.0.0"}
if os.environ.get("MOCK_NO_VERSION") == "1":
    del report["version"]
if os.environ.get("MOCK_EXTRA_FIELDS") == "1":
    # Поля, которые обёртка обязана отрезать: проверочные хеши пароля и
    # ключевой материал из office.info плюс производная от пароля.
    report["info"] = {"passwordSalt": "AAAAAAAAAAAAAAAA",
                      "encryptedVerifierHashValue": "Zm9vYmFyYmF6cXV4",
                      "spinValue": 100000}
    report["secret_key"] = "c2VjcmV0LWtleS1tYXRlcmlhbA=="

if report["decrypted"]:
    with open(out_path, "wb") as sink:
        sink.write(b"MOCK-DECRYPTED-DOCUMENT\n" * 9707)
    report["decrypted_size"] = 9707

sys.stdout.write(json.dumps(report))
