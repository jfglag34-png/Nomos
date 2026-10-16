#!/usr/bin/env python3
"""Mock CLI olevba для веток отказа (без пакета oletools и без документов).

Имитирует `python3 -m oletools.olevba -j -a <file>`: печатает в stdout ТОЧНО такую
же рамку, как настоящий donor, — предупреждение о кодировке (в stdout оно
попадает не всегда, поэтому режим MOCK_ENCODING_WARNING), открывающую `[`,
объект MetaInformation с отступом, затем `,` и объект по файлу, затем `]`.

Значения в успешном ответе — те же, что отдаёт настоящий olevba 0.60.2 на
fixtures/macro.doc: четыре находки (AutoExec/AutoOpen и Suspicious на Shell,
vbHide, powershell) с их формулировками и один макрос Module1.bas. Настоящий
`-a` печатает ещё и `file`, `container`, `do_deobfuscate`, `show_pcode`,
`code_deobfuscated` и `code` — все они здесь присутствуют, чтобы кейс «поле
наружу не уходит» проверял обёртку на настоящем наборе, а не на удобном.

Где применяется: только кейсы, которые настоящим донором не вызвать — зависнуть
или отдать битый ответ он не может. Контракт (порты, типы, значения) проверяют
кейсы без OLETOOLS_BIN: они запускают настоящий olevba по фикстурам из
fixtures/.

Режимы env:
  MOCK_SLEEP=N            — зависнуть на N секунд (ветка таймаута);
  MOCK_FAIL=1             — упасть с кодом 8, как olevba на битом файле;
  MOCK_NO_REPORT=1        — не печатать ничего (ветка no_report);
  MOCK_BAD_REPORT=1       — оборвать JSON (ветка bad_report);
  MOCK_NO_VERSION=1       — MetaInformation без version;
  MOCK_BROKEN_FILE=1      — json_conversion_successful: false;
  MOCK_NO_MACROS=1        — документ без макросов (macros [], analysis null);
  MOCK_NO_ANALYSIS_LIST=1 — analysis не список, а мусор;
  MOCK_ENCODING_WARNING=1 — напечатать предупреждение о кодировке в stderr
                            (как настоящий донор на проверенной машине);
  MOCK_STDOUT_NOISE=1     — положить ту же строку в stdout перед '[';
  MOCK_CODE=1             — положить в code настоящий исходник макроса:
                            обёртка обязана отрезать его молча.
"""
import json
import os
import sys
import time

if os.environ.get("MOCK_SLEEP"):
    time.sleep(float(os.environ["MOCK_SLEEP"]))

args = sys.argv[1:]
filename = args[-1] if args else "sample.doc"
analysis_only = "-a" in args or "--analysis" in args

if os.environ.get("MOCK_FAIL") == "1":
    print("olevba: unable to open the file", file=sys.stderr)
    sys.exit(8)

if os.environ.get("MOCK_NO_REPORT") == "1":
    sys.exit(0)

# Настоящий olevba печатает это предупреждение, и на проверенной машине оно
# уходит в stderr (с 2>/dev/null stdout остаётся чистым JSON). MOCK_STDOUT_NOISE
# кладёт ту же строку в stdout — куда она попадёт, зависит от кодировки
# консоли, а не от контракта донора, и разбор обязан это пережить.
if os.environ.get("MOCK_ENCODING_WARNING") == "1":
    print("Encoding for stdout is only cp1251, will auto-encode text "
          "with utf8 before output", file=sys.stderr)
if os.environ.get("MOCK_STDOUT_NOISE") == "1":
    sys.stdout.write("Encoding for stdout is only cp1251, will auto-encode "
                     "text with utf8 before output")

meta = {
    "script_name": "olevba",
    "version": "0.60.2",
    "python_version": [3, 12, 0],
    "url": "http://decalage.info/python/oletools",
    "type": "MetaInformation",
}
if os.environ.get("MOCK_NO_VERSION") == "1":
    del meta["version"]

code = None
if not analysis_only or os.environ.get("MOCK_CODE") == "1":
    code = 'Attribute VB_Name = "Module1"\r\nSub AutoOpen()\r\nEnd Sub\r\n'

if os.environ.get("MOCK_NO_MACROS") == "1":
    macros = []
    analysis = None
else:
    macros = [{
        "vba_filename": "Module1.bas",
        "subfilename": filename,
        "ole_stream": "VBA/Module1",
        "code": code,
    }]
    analysis = [
        {"type": "AutoExec", "keyword": "AutoOpen",
         "description": "Runs when the Word document is opened"},
        {"type": "Suspicious", "keyword": "Shell",
         "description": "May run an executable file or a system command"},
        {"type": "Suspicious", "keyword": "vbHide",
         "description": "May run an executable file or a system command"},
        {"type": "Suspicious", "keyword": "powershell",
         "description": "May run PowerShell commands"},
    ]

if os.environ.get("MOCK_NO_ANALYSIS_LIST") == "1":
    analysis = {"oops": True}

broken = os.environ.get("MOCK_BROKEN_FILE") == "1"
result = {
    "container": None,
    "file": filename,
    "json_conversion_successful": not broken,
    "analysis": analysis,
    "code_deobfuscated": None,
    "do_deobfuscate": False,
    "show_pcode": False,
    "type": "OLE",
    "macros": macros,
}

if os.environ.get("MOCK_BAD_REPORT") == "1":
    sys.stdout.write("[\n" + json.dumps(meta, indent=4) + ",\n")
    sys.stdout.write('{"file": "x.doc", "macros": [oops}\n')
    sys.exit(0)

# Рамка как у настоящего log_helper: '[' , первый объект с отступом,
# остальные с ',' и отступом, в конце ']'.
sys.stdout.write("[\n")
sys.stdout.write(json.dumps(meta, indent=4) + ",\n")
sys.stdout.write(json.dumps(result, indent=4))
sys.stdout.write("\n]\n")
print("olevba: analysis done", file=sys.stderr)
