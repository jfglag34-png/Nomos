#!/usr/bin/env python3
"""Сборщик фикстур plugins/community/msoffcrypto_tool/fixtures/.

Зачем он нужен. Плагин проверяется на настоящем доноре (msoffcrypto-tool), и
ему нужен НАСТОЯЩИЙ зашифрованный офисный документ. Взять такой документ
неоткуда: в дистрибутивах pip его нет, а качать бинарь из интернета ради
фикстуры нельзя (и лицензия чужая). Поэтому зашифрованный документ собирается
здесь, из открытого текста, САМИМ донором — тем же кодом, которым оператор
зашифровал бы файл.

    python fixtures/make_office.py

Создаёт два файла:

  plain.docx       — незашифрованный OOXML-пакет. Собирается zipfile'ом с
                     зафиксированным mtime (1980-01-01), поэтому байт-в-байт
                     повторяем: zip хранит время каждой записи, и без этого
                     две сборки отличаются.
  encrypted.docx   — plain.docx, зашифрованный донором:
                     `python -m msoffcrypto plain.docx encrypted.docx -p <pass> -e`

Про повторяемость encrypted.docx. Байт-в-байт её НЕТ, и это свойство донора,
а не упущение сборщика: ECMA-376 Agile при шифровании выбирает соль и
spinCount случайно, поэтому два запуска дают разные файлы (проверено: два
прогона подряд — разные md5). Повторяемо то, что важно для тестов: после
расшифровки получается байт-в-байт исходный plain.docx, а разобранные
значения донора (type=agile, encrypted=true, размер открытого текста) не
зависят от того, какая соль выпала. Тесты сверяются с зафиксированным
encrypted.docx, а не пересобираются.

Пароль `Fixture-Passw0rd` — часть фикстуры, а не секрет: он написан здесь
открытым текстом, лежит в plugin.test.yaml и в README плагина. Настоящий
пароль в фикстурах и тестах быть не должен.
"""
import os
import subprocess
import sys
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))

# Фиксированное время записи zip: 1980-01-01 00:00:00 — минимум, который
# умеет хранить формат zip, и он не зависит от машины.
ZIP_DATE = (1980, 1, 1, 0, 0, 0)

FIXTURE_PASSWORD = "Fixture-Passw0rd"

CONTENT_TYPES = """<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>
</Types>
"""

RELS = """<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>
</Relationships>
"""

DOCUMENT = """<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Фикстура msoffcrypto_tool: документ без макросов и без
    секретов, который после расшифровки должен совпасть байт-в-байт.</w:t></w:r></w:p>
  </w:body>
</w:document>
"""

# Набивка нужна, чтобы пакет был больше одного сегмента шифрования (4096 байт).
# Проверено на msoffcrypto-tool 6.0.0: ECMA-376 Agile делит открытый текст на
# сегменты по 4096 байт, и при ОДНОМ сегментеencrypt() даёт файл, который сам же
# decrypt() расшифровать не может (проверено на пакетах 403, 803, 1303, 1384,
# 1391 и 2303 байта — все четыре: InvalidKeyError или «длина не кратна длине
# блока»; начиная с 4399 байт round-trip сходится). Настоящие .docx больше
# 4 КБ всегда, поэтому набивка не искажает то, что мы проверяем, — она просто
# выводит фикстуру из этой поломанной ветки. Содержимое — детерминированный
# текст, чтобы размер и байты пакета не зависели от генератора случайных чисел.
FILLER_LINES = 120
FILLER = "".join("строка набивки %04d: "
                 "abcdefghijklmnopqrstuvwxyz0123456789\n" % i
                 for i in range(FILLER_LINES))


def build_plain(path):
    """Минимальный валидный .docx. Порядок записей зафиксирован, он тоже
    часть байтов файла. Набивка кладётся БЕЗ сжатия: сжиматый повторяющийся
    текст весит сотни байт вместо шести тысяч, и пакет снова уезжает в
    однопакетный (сломанный) режим шифрования."""
    parts = [
        ("[Content_Types].xml", CONTENT_TYPES),
        ("_rels/.rels", RELS),
        ("word/document.xml", DOCUMENT),
    ]
    with zipfile.ZipFile(path, "w", zipfile.ZIP_DEFLATED) as zf:
        for name, data in parts:
            info = zipfile.ZipInfo(name, date_time=ZIP_DATE)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o600 << 16
            zf.writestr(info, data.encode("utf-8"))
        info = zipfile.ZipInfo("word/filler.txt", date_time=ZIP_DATE)
        info.compress_type = zipfile.ZIP_STORED
        info.external_attr = 0o600 << 16
        zf.writestr(info, FILLER.encode("utf-8"))


def encrypt(plain, encrypted, password):
    # Донор зовётся как модуль (python -m msoffcrypto), а не как console-script:
    # pip кладёт msoffcrypto-tool.exe в каталог Scripts, который на Windows часто
    # не в PATH, и тогда «донор установлен и при этом не найден».
    cmd = [sys.executable, "-m", "msoffcrypto", plain, encrypted,
           "-p", password, "-e"]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        sys.stderr.write(proc.stdout or "")
        sys.stderr.write(proc.stderr or "")
        raise SystemExit("msoffcrypto не зашифровал: exit %d" % proc.returncode)


def main():
    plain = os.path.join(HERE, "plain.docx")
    encrypted = os.path.join(HERE, "encrypted.docx")
    build_plain(plain)
    encrypt(plain, encrypted, FIXTURE_PASSWORD)
    print("plain.docx     %d байт" % os.path.getsize(plain))
    print("encrypted.docx %d байт" % os.path.getsize(encrypted))


if __name__ == "__main__":
    main()
