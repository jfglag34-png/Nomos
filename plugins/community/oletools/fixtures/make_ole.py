#!/usr/bin/env python3
"""Сборщик фикстур plugins/community/oletools/fixtures/.

Фикстуры — настоящие OLE-контейнеры (CFB, [MS-CFB]), собранные здесь с нуля.
Зачем: плагин проверяется на живом доноре olevba, а документ с макросом взять
неоткуда — в дистрибутивах pip oletools его нет, а качать бинарь из интернета
ради фикстуры нельзя (и лицензия чужая). Свой файл вместо чужого ещё и
честнее: его видно целиком, и в нём нет ничего, кроме текста, напечатанного
ниже.

    python fixtures/make_ole.py

Три файла:

  macro.doc      OLE с VBA-проектом: PROJECT, VBA/_VBA_PROJECT, VBA/dir,
                 VBA/Module1. Исходник макроса напечатан в VBA_SOURCE.
  nomacros.doc   OLE-документ БЕЗ макросов (поток WordDocument). Нужен, чтобы
                 «макросов нет» было проверено живым доно��ором, а не моком:
                 ноль находок — это результат, а не ошибка.
  truncated.doc  первые 1024 байта macro.doc. Каталог обещает секторы за концом
                 файла; на этом настоящий olevba падает (exit 8), и это
                 проверяемая ветка отказа.

Что внутри macro.doc (видно и в исходнике, и в выводе olevba):

  PROJECT              текстовый поток с объявлением модуля Module1
  VBA/_VBA_PROJECT     заглушка: olevba проверяет только наличие потока
  VBA/dir              сжатый (MS-OVBA 2.4.1) каталог проекта: один модуль
  VBA/Module1          сжатый исходник макроса

Три вещи, которые здесь сделаны не так, как их делает Office, и почему:

  * Сжатие в dir и в Module1 — «все токены литеральные»: данные режутся на
    куски по 3600 байт (не 4096: восемь литералов стоят девять байт, и блок в
    4096 байт не влезает в сжатый чанк размером 4098 — ровно эта граница
    проверяется в decompress_stream). Office сжимает RLE-копиями, но читатель
    обязан понимать оба вида, а проверяем мы читателя.
  * MODULEOFFSET = 0: исходник начинается с первого байта потока модуля. У
    настоящих файлов там «кэш производительности» VBE — его пришлось бы
    выдумывать, а читателю он не нужен.
  * Каталог директорий — вырожденное дерево (соседи только через right).
    olefile обходит дерево по child/left/right и красно-чёрные свойства не
    проверяет, а записей здесь шесть.

Детерминированность: файл собирается байт-в-байт повторяемо — времени
создания, CLSID и содержимого PerformanceCache в нём нет, а всё остальное
записано константами этого файла.
"""
import os
import struct

HERE = os.path.dirname(os.path.abspath(__file__))

SECTOR_SIZE = 512
MINI_SECTOR_SIZE = 64
MINI_STREAM_CUTOFF = 4096
DIR_ENTRY_SIZE = 128
NOSTREAM = 0xFFFFFFFF
ENDOFCHAIN = 0xFFFFFFFE
FREESECT = 0xFFFFFFFF
FATSECT = 0xFFFFFFFD

# Кодовая страница проекта: 1252 (Windows-1252). Исходник макроса поэтому
# ASCII — при другой кодовой странице он декодировался бы иначе.
CODEPAGE = 1252

MODULE_NAME = "Module1"
PROJECT_NAME = "VBAProject"

# Исходник макроса. Подобран так, чтобы сработали ключи, на которые olevba
# смотрит сам: AutoOpen — автозапуск, Shell/vbHide/powershell — подозрительное
# поведение. Никакой вредоносности: это текст, ничего не выполняется.
VBA_SOURCE = (
    'Attribute VB_Name = "Module1"\r\n'
    "Sub AutoOpen()\r\n"
    '    MsgBox "fixture oletools"\r\n'
    "End Sub\r\n"
    "\r\n"
    "Sub RunHidden()\r\n"
    "    Dim cmd As String\r\n"
    '    cmd = "powershell -enc SQBFAFgA"\r\n'
    "    Shell cmd, vbHide\r\n"
    "End Sub\r\n"
)

PROJECT_STREAM = (
    'ID="{5DD90D76-4FC0-4A1B-9B33-1E0A4C1D7E52}"\r\n'
    "Document=ThisDocument/&H00000000\r\n"
    "Module=%s\r\n"
    'Name="%s"\r\n'
    'HelpContextID="0"\r\n'
    'VersionCompatible32="393222000"\r\n'
    "\r\n"
    "[Host Extender Info]\r\n"
    "&H00000001={3832D640-CF90-11CF-8E43-00A0C911005A};VBE;&H00000000\r\n"
    "\r\n"
    "[Workspace]\r\n"
    "ThisDocument=0, 0, 0, 0, C\r\n"
    "%s=0, 0, 0, 0, C\r\n" % (MODULE_NAME, PROJECT_NAME, MODULE_NAME)
)


# ── сжатие MS-OVBA 2.4.1 ────────────────────────────────────────────────────
def ovba_compress(data):
    """Контейнер MS-OVBA, где каждый токен — литерал.

    Сигнатура 0x01, затем чанки. Заголовок чанка: бит 15 — флаг «сжатый»,
    биты 12-14 — подпись 0b011, биты 0-11 — размер чанка минус 3.
    """
    out = bytearray(b"\x01")
    if not data:
        return bytes(out)
    for start in range(0, len(data), 3600):
        chunk = data[start:start + 3600]
        body = bytearray()
        for i in range(0, len(chunk), 8):
            body.append(0x00)              # флаг-байт: восемь литералов
            body += chunk[i:i + 8]
        size = len(body) + 2               # заголовок (2) + тело
        out += struct.pack("<H", 0xB000 | (size - 3)) + body
    return bytes(out)


# ── поток dir ──────────────────────────────────────────────────────────────
def rec(rid, payload):
    """Запись MS-OVBA: Id (2 байта), Size (4 байта), содержимое."""
    return struct.pack("<HI", rid, len(payload)) + payload


def build_dir_stream():
    name = MODULE_NAME.encode("cp1252")
    name16 = MODULE_NAME.encode("utf-16-le")
    out = bytearray()
    out += rec(0x0001, struct.pack("<I", 1))            # PROJECTSYSKIND
    out += rec(0x0002, struct.pack("<I", 0x409))       # PROJECTLCID
    out += rec(0x0014, struct.pack("<I", 0x409))       # PROJECTLCIDINVOKE
    out += struct.pack("<HIH", 0x0003, 2, CODEPAGE)    # PROJECTCODEPAGE
    out += rec(0x0004, name)                           # PROJECTNAME
    out += rec(0x0005, b"")                            # PROJECTDOCSTRING
    out += struct.pack("<HI", 0x0040, 0)               #   DocStringUnicode
    out += rec(0x0006, b"")                            # PROJECTHELPFILEPATH
    out += struct.pack("<HI", 0x003D, 0)               #   HelpFile2
    out += rec(0x0007, struct.pack("<I", 0))           # PROJECTHELPCONTEXT
    out += rec(0x0008, struct.pack("<I", 0))           # PROJECTLIBFLAGS
    out += struct.pack("<HIIH", 0x0009, 4, 1, 0)       # PROJECTVERSION
    out += rec(0x000C, b"")                            # PROJECTCONSTANTS
    out += struct.pack("<HI", 0x003C, 0)               #   ConstantsUnicode
    # Ссылок на библиотеки нет: цикл парсера читает Id и выходит на 0x000F.
    out += struct.pack("<HIH", 0x000F, 2, 1)          # PROJECTMODULES
    out += struct.pack("<HIH", 0x0013, 2, 0xFFFF)      #   ProjectCookieRecord
    out += rec(0x0019, name)                           # MODULENAME
    out += rec(0x0047, name16)                         # MODULENAMEUNICODE
    out += rec(0x001A, name)                           # MODULESTREAMNAME
    out += struct.pack("<HI", 0x0032, len(name16)) + name16
    out += rec(0x001C, b"")                            # MODULEDOCSTRING
    out += struct.pack("<HI", 0x0048, 0)
    out += struct.pack("<HII", 0x0031, 4, 0)           # MODULEOFFSET
    out += rec(0x001E, struct.pack("<I", 0))           # MODULEHELPCONTEXT
    out += struct.pack("<HIH", 0x002C, 2, 0xFFFF)      # MODULECOOKIE
    out += struct.pack("<HI", 0x0021, 0)               # MODULETYPE: модуль
    out += struct.pack("<HI", 0x002B, 0)               # MODULEEND
    out += struct.pack("<HI", 0x0010, 0)               # терминатор проекта
    return ovba_compress(bytes(out))


# ── контейнер CFB ──────────────────────────────────────────────────────────
def dir_entry(name, etype, left=NOSTREAM, right=NOSTREAM, child=NOSTREAM,
              start=ENDOFCHAIN, size=0):
    raw = name.encode("utf-16-le") + b"\x00\x00"
    if len(raw) > 64:
        raise ValueError("имя записи длиннее 31 символа: %r" % name)
    raw += b"\x00" * (64 - len(raw))
    return (raw
            + struct.pack("<H", len(name) * 2 + 2)   # длина имени с NUL
            + struct.pack("<BB", etype, 1)           # тип записи и цвет
            + struct.pack("<III", left, right, child)
            + b"\x00" * 16                           # CLSID
            + struct.pack("<I", 0)                    # state bits
            + b"\x00" * 16                           # время создания/изменения
            + struct.pack("<I", start)
            + struct.pack("<Q", size))


def write_cfb(path, entries):
    """Собирает CFB v3 (сектор 512, мини-сектор 64) по таблице записей.

    entries — список (name, etype, data, tree): etype 5 = корень, 1 = storage,
    2 = stream; tree задаёт left/right/child. Потоки меньше порога мини-потока
    (4096) лежат в мини-потоке, который принадлежит записи корня.
    """
    # 1. Мини-поток: маленькие потоки идут в него, по 64 байта на мини-сектор.
    mini = bytearray()
    mini_start = {}
    for sid, (name, etype, data, tree) in enumerate(entries):
        if etype != 2 or not data:
            continue
        if len(data) >= MINI_STREAM_CUTOFF:
            raise ValueError("поток %r крупнее порога мини-потока" % name)
        mini_start[sid] = len(mini) // MINI_SECTOR_SIZE
        mini += data
        if len(mini) % MINI_SECTOR_SIZE:
            mini += b"\x00" * (MINI_SECTOR_SIZE - len(mini) % MINI_SECTOR_SIZE)
    n_mini_sectors = len(mini) // MINI_SECTOR_SIZE

    # 2. MiniFAT: цепочки мини-секторов каждого потока.
    minifat = [FREESECT] * n_mini_sectors
    for sid, (name, etype, data, tree) in enumerate(entries):
        if etype != 2 or not data:
            continue
        first = mini_start[sid]
        count = (len(data) + MINI_SECTOR_SIZE - 1) // MINI_SECTOR_SIZE
        for k in range(count):
            minifat[first + k] = first + k + 1 if k + 1 < count else ENDOFCHAIN
    minifat_bytes = b"".join(struct.pack("<I", v) for v in minifat)
    if len(minifat_bytes) % SECTOR_SIZE:
        minifat_bytes += b"\xff" * (SECTOR_SIZE - len(minifat_bytes) % SECTOR_SIZE)

    # 3. Раскладка секторов: каталог, мини-поток, MiniFAT, FAT — подряд с нуля.
    n_dir = (len(entries) * DIR_ENTRY_SIZE + SECTOR_SIZE - 1) // SECTOR_SIZE
    ministream_start = n_dir
    n_ministream = (len(mini) + SECTOR_SIZE - 1) // SECTOR_SIZE
    minifat_start = n_dir + n_ministream
    n_minifat = len(minifat_bytes) // SECTOR_SIZE
    n_data = n_dir + n_ministream + n_minifat

    # 4. Сколько секторов FAT — итерация до неподвижности.
    n_fat = 1
    while True:
        need = (n_data + n_fat + SECTOR_SIZE // 4 - 1) // (SECTOR_SIZE // 4)
        if need <= n_fat:
            break
        n_fat = need
    fat_start = n_data

    fat = [FREESECT] * (n_data + n_fat)

    def chain(start, count):
        for k in range(count):
            fat[start + k] = start + k + 1 if k + 1 < count else ENDOFCHAIN

    chain(0, n_dir)
    chain(ministream_start, n_ministream)
    chain(minifat_start, n_minifat)
    for k in range(n_fat):
        fat[fat_start + k] = FATSECT
    fat_bytes = b"".join(struct.pack("<I", v) for v in fat)
    fat_bytes += b"\xff" * (n_fat * SECTOR_SIZE - len(fat_bytes))

    # 5. Заголовок.
    header = bytearray()
    header += b"\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"     # сигнатура
    header += b"\x00" * 16                            # CLSID
    header += struct.pack("<HH", 0x003E, 0x0003)      # minor/major версия
    header += struct.pack("<H", 0xFFFE)               # порядок байтов
    header += struct.pack("<HH", 9, 6)                # сдвиги сектора и мини
    header += b"\x00" * 6                             # зарезервировано
    header += struct.pack("<I", 0)                    # число секторов каталога
    header += struct.pack("<I", n_fat)                # число секторов FAT
    header += struct.pack("<I", 0)                    # первый сектор каталога
    header += struct.pack("<I", 0)                    # transaction signature
    header += struct.pack("<I", MINI_STREAM_CUTOFF)   # порог мини-потока
    header += struct.pack("<I", minifat_start)        # первый сектор MiniFAT
    header += struct.pack("<I", n_minifat)            # число секторов MiniFAT
    header += struct.pack("<I", ENDOFCHAIN)           # первый сектор DIFAT
    header += struct.pack("<I", 0)                    # число секторов DIFAT
    for k in range(109):
        header += struct.pack("<I", fat_start + k if k < n_fat else FREESECT)
    if len(header) != SECTOR_SIZE:
        raise AssertionError("заголовок CFB не 512 байт: %d" % len(header))

    # 6. Записи каталога. У корня start — сектор мини-потока в FAT, у потоков —
    #    номер МИНИ-сектора, цепочка которого лежит в MiniFAT; у storage обоих
    #    нет, поэтому start = ENDOFCHAIN.
    blobs = []
    for sid, (name, etype, data, tree) in enumerate(entries):
        if etype == 5:
            start, size = ministream_start, len(mini)
        elif etype == 2 and data:
            start, size = mini_start[sid], len(data)
        else:
            start, size = ENDOFCHAIN, 0
        blobs.append(dir_entry(name, etype, start=start, size=size, **tree))
    dir_bytes = b"".join(blobs)
    if len(dir_bytes) % SECTOR_SIZE:
        dir_bytes += b"\x00" * (SECTOR_SIZE - len(dir_bytes) % SECTOR_SIZE)

    mini_bytes = bytes(mini)
    if len(mini_bytes) % SECTOR_SIZE:
        mini_bytes += b"\x00" * (SECTOR_SIZE - len(mini_bytes) % SECTOR_SIZE)

    out = bytes(header) + dir_bytes + mini_bytes + minifat_bytes + fat_bytes
    with open(path, "wb") as sink:
        sink.write(out)
    return out


def build_macro_doc(path):
    vba_project = (b"\xcc\x61"          # Reserved1
                   b"\xff\xff"         # Version
                   b"\x00"             # Reserved2
                   b"\x00\x00"         # Reserved3
                   + b"\x00" * 16)     # PerformanceCache: не читается
    entries = [
        ("Root Entry", 5, None, dict(child=1)),
        ("PROJECT", 2, PROJECT_STREAM.encode("cp1252"), dict(right=2)),
        ("VBA", 1, b"", dict(child=3)),
        ("_VBA_PROJECT", 2, vba_project, dict(right=4)),
        ("dir", 2, build_dir_stream(), dict(right=5)),
        (MODULE_NAME, 2,
         ovba_compress(VBA_SOURCE.encode("cp1252")), dict()),
    ]
    return write_cfb(path, entries)


def build_nomacros_doc(path):
    # Документ без макросов: один поток WordDocument, storage VBA нет вовсе.
    entries = [
        ("Root Entry", 5, None, dict(child=1)),
        ("WordDocument", 2, b"\x00" * 512, dict()),
    ]
    return write_cfb(path, entries)


def main():
    macro = build_macro_doc(os.path.join(HERE, "macro.doc"))
    nomacros = build_nomacros_doc(os.path.join(HERE, "nomacros.doc"))
    truncated = macro[:1024]
    with open(os.path.join(HERE, "truncated.doc"), "wb") as sink:
        sink.write(truncated)
    print("macro.doc     %d байт" % len(macro))
    print("nomacros.doc  %d байт" % len(nomacros))
    print("truncated.doc %d байт" % len(truncated))


if __name__ == "__main__":
    main()
