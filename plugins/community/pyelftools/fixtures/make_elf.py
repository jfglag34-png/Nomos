#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Генератор ELF-фикстур для plugins/community/pyelftools.

Зачем он здесь. Обёртка проверяется на НАСТОЯЩЕМ доноре, а настоящий ELF нужно
где-то взять. На этой машине (Windows) нет кросс-тулчейна, который собрал бы
Linux-бинарник: mingw даёт PE, а не ELF. Поэтому файл собирается прямо здесь,
побайтно, по спецификации System V ABI (ELF64/ELF32) — и рядом лежит он сам,
чтобы собранное можно было перепроверить, а не принять на веру:

    python fixtures/make_elf.py          # пересобрать все три файла

Чем проверялось, что фикстура настоящая, а не просто «файл с магией»:

  * readelf -aS -l -d (binutils из mingw-w64) — разбирает без предупреждений;
  * pyelftools 0.33 — читает все секции, оба тега DT_NEEDED и символы;
  * LIEF 1.0.0 — видит формат ELF, обе секции и зависимости.

Что в файлах (все значения выдуманы, ничего боевого тут нет):

  libfixture.so   ET_DYN, ELF64, x86-64. 7 секций, DT_NEEDED libc.so.6 и
                  libpthread.so.0, DT_SONAME libfixture.so, два символа.
                  Нужен именно динамический ELF: у статического бинарника нет
                  секции .dynamic, и порт libraries нечего было бы проверять.
  fixture32.o     ET_REL, ELF32, i386 — три секции, DT_NEEDED нет. Показывает,
                  что elf_class и набор секций приходят от донора, а не от
                  обёртки: для одного и того же плагина тут другой класс и
                  другой ET_.
  truncated.so    первые 60% libfixture.so. Заголовок обещает таблицу секций
                  за концом файла — донор обязан на этом упасть, и обёртка
                  обязана отличить «файл не ELF» от «ELF, но битый».

Файлы детерминированы: ни времени сборки, ни случайных байт, ни имён
пользователя в них нет, поэтому пересборка даёт байт в байт тот же файл.
"""
import os
import struct
import sys

# ── константы формата ────────────────────────────────────────────────────────
ELFCLASS32, ELFCLASS64 = 1, 2
ELFDATA2LSB = 1
EM_386, EM_X86_64 = 3, 62
ET_REL, ET_DYN = 1, 3
PT_LOAD, PT_DYNAMIC = 1, 2
PF_X, PF_W, PF_R = 1, 2, 4
SHT_PROGBITS, SHT_SYMTAB = 1, 2
SHT_STRTAB, SHT_DYNAMIC, SHT_NOBITS = 3, 6, 8
SHT_DYNSYM = 11
SHF_WRITE, SHF_ALLOC, SHF_EXECINSTR = 0x1, 0x2, 0x4
STB_GLOBAL, STT_FUNC, STT_OBJECT = 1, 2, 1
DT_NULL, DT_NEEDED, DT_STRTAB, DT_SYMTAB = 0, 1, 5, 6
DT_STRSZ, DT_SYMENT, DT_SONAME, DT_DEBUG = 10, 11, 14, 21

# Содержимое .text: endbr64 + пара инструкций + ret. Настоящий код не нужен —
# нужен файл, который донор считает исполняемой секцией.
TEXT64 = bytes([
    0xF3, 0x0F, 0x1E, 0xFA,                    # endbr64
    0x48, 0x8D, 0x05, 0x0D, 0x00, 0x00, 0x00,  # lea rax, [rip+13]
    0xB8, 0x07, 0x00, 0x00, 0x00,              # mov eax, 7
    0xC3,                                      # ret
    0x90, 0x90, 0x90, 0x90, 0x90,              # nop-подушка
]) + bytes(range(0x40, 0x60))                  # «данные» внутри .text
TEXT32 = bytes([0x31, 0xC0, 0xC3]) + bytes(13)  # xor eax,eax; ret; выравнивание
DATA64 = b"WEDRAFIXT" + bytes(7)
DATA32 = b"WEDRAFIXT" + bytes(5)


def align_up(value, alignment):
    return (value + alignment - 1) // alignment * alignment


class Blob:
    """Файл собирается как список кусков, адреса считаются по ходу."""

    def __init__(self):
        self.buf = bytearray()

    def tell(self):
        return len(self.buf)

    def pad_to(self, alignment):
        self.buf += bytes(align_up(len(self.buf), alignment) - len(self.buf))

    def put(self, data):
        self.buf += data


def strtab(entries):
    """Словарь строк: {имя: смещение}. Смещение 0 — пустая строка."""
    blob = bytearray(b"\x00")
    offsets = {"": 0}
    for name in entries:
        offsets[name] = len(blob)
        blob += name.encode("utf-8") + b"\x00"
    return bytes(blob), offsets


def build_elf64():
    dynstr, so = strtab(["libc.so.6", "libpthread.so.0", "libfixture.so",
                         "wedra_fixture_answer", "wedra_fixture_data"])
    shstr, sh = strtab([".dynstr", ".dynsym", ".text", ".dynamic", ".data",
                        ".bss", ".shstrtab"])

    blob = Blob()
    # e_ident: 4 байта магии + CLASS, DATA, VERSION, OSABI, ABIVERSION + 7
    # байт выравнивания = ровно 16. На 15 байтах (забытый EI_ABIVERSION) все
    # адреса после заголовка съезжают на байт, и бинарник «почти ELF».
    blob.put(b"\x7fELF" + bytes([ELFCLASS64, ELFDATA2LSB, 1, 0, 0]) + bytes(7))
    ehdr_size = 64
    phnum = 4
    phoff = ehdr_size
    blob.put(bytes(ehdr_size - 16))  # место под rest заголовка, заполним в конце

    # ── таблица программных заголовков (заполняется в конце, когда известны
    #    смещения; здесь только резервируем) ──
    phdr_off = blob.tell()
    blob.put(bytes(56 * phnum))

    # ── .dynstr ──
    dynstr_off = blob.tell()
    blob.put(dynstr)
    dynstr_addr = dynstr_off

    # ── .dynsym: null + два символа ──
    blob.pad_to(8)
    dynsym_off = blob.tell()
    dynsym_addr = dynsym_off
    symbols = [
        (0, 0, 0, 0, 0),                                          # STN_UNDEF, SHN_UNDEF=0
        (so["wedra_fixture_answer"], STB_GLOBAL << 4 | STT_FUNC, 3, 0, 32),
        (so["wedra_fixture_data"], STB_GLOBAL << 4 | STT_OBJECT, 5, 0, 16),
    ]
    for name, info, shndx, value, size in symbols:
        blob.put(struct.pack("<IBBHQQ", name, info, 0, shndx, value, size))
    dynsym_size = blob.tell() - dynsym_off

    # ── .text ──
    blob.pad_to(16)
    text_off = blob.tell()
    blob.put(TEXT64)
    text_addr = text_off
    text_size = len(TEXT64)

    # ── .dynamic ──
    blob.pad_to(8)
    dyn_off = blob.tell()
    dyn_addr = dyn_off
    dyn = [
        (DT_NEEDED, so["libc.so.6"]),
        (DT_NEEDED, so["libpthread.so.0"]),
        (DT_SONAME, so["libfixture.so"]),
        (DT_STRTAB, dynstr_addr),
        (DT_SYMTAB, dynsym_addr),
        (DT_STRSZ, len(dynstr)),
        (DT_SYMENT, 24),
        (DT_DEBUG, 0),
        (DT_NULL, 0),
    ]
    for tag, val in dyn:
        blob.put(struct.pack("<qQ", tag, val))
    dyn_size = blob.tell() - dyn_off

    # ── .data ──
    blob.pad_to(8)
    data_off = blob.tell()
    blob.put(DATA64)
    data_addr = data_off
    data_size = len(DATA64)

    # ── .bss (NOBITS): в файле места нет, в памяти — 64 байта ──
    blob.pad_to(16)
    bss_addr = blob.tell()
    bss_size = 64

    # ── .shstrtab ──
    shstr_off = blob.tell()
    blob.put(shstr)
    shstr_size = len(shstr)

    # ── таблица секций ──
    blob.pad_to(8)
    shoff = blob.tell()
    sections = [
        # name, type, flags, addr, offset, size, link, info, align, entsize
        ("", 0, 0, 0, 0, 0, 0, 0, 0, 0),
        (".dynstr", SHT_STRTAB, SHF_ALLOC, dynstr_addr, dynstr_off,
         len(dynstr), 0, 0, 1, 0),
        (".dynsym", SHT_DYNSYM, SHF_ALLOC, dynsym_addr,
         dynsym_off, dynsym_size, 1, 1, 8, 24),
        (".text", SHT_PROGBITS, SHF_ALLOC | SHF_EXECINSTR, text_addr,
         text_off, text_size, 0, 0, 16, 0),
        (".dynamic", SHT_DYNAMIC, SHF_ALLOC | SHF_WRITE, dyn_addr, dyn_off,
         dyn_size, 1, 0, 8, 16),
        (".data", SHT_PROGBITS, SHF_ALLOC | SHF_WRITE, data_addr, data_off,
         data_size, 0, 0, 8, 0),
        (".bss", SHT_NOBITS, SHF_ALLOC | SHF_WRITE, bss_addr, bss_addr,
         bss_size, 0, 0, 16, 0),
        (".shstrtab", SHT_STRTAB, 0, 0, shstr_off, shstr_size, 0, 0, 1, 0),
    ]
    for name, stype, flags, addr, off, size, link, info, al, entsize in sections:
        blob.put(struct.pack("<IIQQQQIIQQ", sh[name], stype, flags, addr, off,
                             size, link, info, al, entsize))
    shoff_size = blob.tell() - shoff

    # ── программные заголовки ──
    phdrs = [
        (PT_LOAD, PF_R, 0, 0, 0, dynsym_off + dynsym_size,
         dynsym_off + dynsym_size, 0x1000),
        (PT_LOAD, PF_R | PF_X, text_off, text_off, text_off, text_size,
         text_size, 0x1000),
        (PT_LOAD, PF_R | PF_W, data_off, data_addr, data_addr, data_size,
         data_size + bss_size, 0x1000),
        (PT_DYNAMIC, PF_R | PF_W, dyn_off, dyn_addr, dyn_addr, dyn_size,
         dyn_size, 8),
    ]
    pos = phdr_off
    for ptype, flags, off, vaddr, paddr, filesz, memsz, palign in phdrs:
        packed = struct.pack("<IIQQQQQQ", ptype, flags, off, vaddr, paddr,
                             filesz, memsz, palign)
        blob.buf[pos:pos + len(packed)] = packed
        pos += len(packed)

    # ── заголовок ELF64 ──
    rest = struct.pack("<HHIQQQIHHHHHH", ET_DYN, EM_X86_64, 1, text_addr,
                       phoff, shoff, 0, 64, 56, phnum, 64, len(sections),
                       len(sections) - 1)
    blob.buf[16:16 + len(rest)] = rest
    assert shoff_size == 64 * len(sections), shoff_size
    return bytes(blob.buf)


def build_elf32():
    shstr, sh = strtab([".text", ".shstrtab"])
    blob = Blob()
    blob.put(b"\x7fELF" + bytes([ELFCLASS32, ELFDATA2LSB, 1, 0, 0]) + bytes(7))
    blob.put(bytes(52 - 16))

    blob.pad_to(4)
    text_off = blob.tell()
    blob.put(TEXT32)
    text_size = len(TEXT32)

    shstr_off = blob.tell()
    blob.put(shstr)
    shstr_size = len(shstr)

    blob.pad_to(4)
    shoff = blob.tell()
    sections = [
        ("", 0, 0, 0, 0, 0, 0, 0, 0, 0),
        (".text", SHT_PROGBITS, SHF_ALLOC | SHF_EXECINSTR, text_off, text_off,
         text_size, 0, 0, 4, 0),
        (".shstrtab", SHT_STRTAB, 0, 0, shstr_off, shstr_size, 0, 0, 1, 0),
    ]
    for name, stype, flags, addr, off, size, link, info, al, entsize in sections:
        blob.put(struct.pack("<IIIIIIIIII", sh[name], stype, flags, addr, off,
                             size, link, info, al, entsize))

    rest = struct.pack("<HHIIIIIHHHHHH", ET_REL, EM_386, 1, 0, 0, shoff, 0, 52,
                       32, 0, 40, len(sections), len(sections) - 1)
    blob.buf[16:16 + len(rest)] = rest
    return bytes(blob.buf)


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    so64 = build_elf64()
    rel32 = build_elf32()
    files = {
        "libfixture.so": so64,
        "fixture32.o": rel32,
        # 60% файла: заголовок обещает таблицу секций за концом
        "truncated.so": so64[:len(so64) * 3 // 5],
    }
    for name, data in files.items():
        path = os.path.join(here, name)
        with open(path, "wb") as f:
            f.write(data)
        print("%-14s %5d байт  %s" % (name, len(data), path))
    return 0


if __name__ == "__main__":
    sys.exit(main())