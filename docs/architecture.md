# Архитектура — WEDRA

Короткий проводник для нового читателя. Контракт — `protocol/v0.2/PROTOCOL.md`,
здесь — как устроено ядро и почему.

## Принципы

1. **Плагин = процесс.** stdin JSON → stdout JSON, exit code — класс результата.
   Ядро не знает, что внутри плагина (python/го/любой скрипт). Контракт честный:
   «каждый кусок можно независимо написать, протестировать и заменить».
2. **Локально и детерминированно.** Журнал каждого рана — на диске
   (`var/runs/<id>/`), `--resume` собирает прерванный батч. Ничего не живёт
   только в памяти.
3. **Человек в петле — не гостевая фича.** `core/human_gate` — встроенная нода
   ядра (не плагин): форма из контекста, правки материализуются под
   `steps.<gate>.*`; `--yes` авто-принимает только гейты без `approval: human` и без `pipeline.gates: human_only`.
   Встроенные модули `core/*` исполняются в процессе ядра и образуют закрытый
   реестр: наряду с гейтом там `core/text_stats` (метрики текста), благодаря
   чему первая цепочка запускается на одном бинарнике — без клона репозитория и
   без Python. Полный список и правила namespace — в [PROTOCOL.md](../protocol/v0.2/PROTOCOL.md) §7a.
4. **Доверие проверяется до любого эффекта.** `validate` — статика (типы,
   форматы, пути, циклы, сети при `network: deny`, secrets), `registry validate`
   — конформность каждой записи реестра (CI-гейт).

## Дерево

```
cmd/wedra/            # основной CLI
cmd/wedragui/         # desktop launcher
cmd/tool/             # compatibility CLI
internal/
  pipeline/           # model, parser, validation, DAG, when
  execution/          # runner, resume, foreach and parallel phases
  plugin/             # manifest, subprocess, transport, contract
  registry/           # registry format, install, commit pins
  journal/            # append-only journal and stores
  gate/               # human gate
  runctx/             # shared context and dot-paths
  cli/                # WEDRA command adapter
  api/                # HTTP/GUI adapter
  mcp/                # MCP adapter
  core/               # transitional compatibility layer
  common/             # small shared helpers
plugins/official/     # maintained core plugins
plugins/community/    # everything outside the maintained core
examples/             # canonical examples and registry presets
conformance/fixtures/ # public conformance corpus
protocol/             # VERSION (0.2), changelog, v0.2/
schemas/              # pipeline and plugin schemas
docs/                 # architecture, versioning, guides
var/runs/             # runtime output, ignored by git
```

## Layout freeze and compatibility

The directory layout above is the canonical repository layout. The
`internal/core` package and `cmd/tool` are compatibility surfaces: new
implementation logic belongs in the owning package, while existing imports
remain until a separately reviewed migration removes them. The desktop
launcher may use its documented standalone layout next to the executable; it
does not redefine the repository layout.

Until a public RFC/ADR is accepted, do not add parallel top-level package
families, rename canonical directories, or change the meaning of
`examples/`, `plugins/official/`, `plugins/community/`, `protocol/`, or
`var/runs/`. Compatibility changes require a migration note and a regression
test.

The conformance fixture source is `conformance/fixtures/v0.2/`; the internal
copy exists only for development tests and must not become a second public
contract.


## Исполнение рана

```
validate (статика) → run:
  secrets preflight → network preflight (deny)
  → [pipeline-foreach: pre-фаза (steps.* источник)]
  → цикл по элементам (или один «элемент» без foreach):
      сегменты шагов: одиночные | parallel_group (goroutine + барьер)
        каждый шаг: when? → [step-foreach: мини-цикл] → runStep
          runStep: manifest → buildInput (bind > from) → subprocess
                   → enforce output → ctx.SetStep / политики on_error
  → post-фаза (after_foreach, агрегаты steps.<id>_all)
  journal.jsonl на каждом переходе; context.json — снапшоты
```

Параллельные ветки работают на **копиях контекста**; слияние после барьера —
в порядке списка шагов (кто быстрее, не влияет на результат).

## Управляющий поток (v0.20, PROTOCOL §12)

- `when:` — условие (строка = «истинно?» или `{path, op, value}`); ложь = `skipped`.
- `foreach:` на шаге — per-item мини-цикл; `steps.<id>_all` — агрегат; stop = стоп рана.
- `parallel_group` — параллельные ветки с барьером; human_gate в группах запрещён.
- Diamond (A,B → C) — из коробки. **Циклы — сознательно НЕ поддерживаются**:
  цикл принадлежит плагину (он сам держит бюджет итераций); ядро блокирует
  циклы в графе (detect-cycle) — решение и причина в PROTOCOL §10.

## Версии

- `VERSION` (корень) — единственная версия приложения; текущий релиз —
  `0.34.0`, а release tag (`vX.Y.Z`) обязан точно совпадать с `VERSION`.
  Схема — SemVer: она же требуется, чтобы модуль ставился через `go install`
  (Go не считает версией модуля тег вида `v0.33c`).
- `protocol/VERSION` — отдельная версия протокола (`0.2`).
- `format_version` в `pipeline.yaml` и `platform_api` в `plugin.yaml` — поля
  совместимости, не product version.
- Полные правила и исторические aliases описаны в `docs/versioning.md`.
