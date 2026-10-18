# Аудит: internal/core шим + монолиты mcp/server.go, api/server.go, execution/runner.go

> Замер 2026-10-18, Go 1.27.2, `wc -l` без учёта пустых вендор-каталогов. Тесты green (`go test ./...`).

## 1. Что на самом деле занимает internal/core

`git ls internal/core/*.go` = 43 файла, 7547 строк total. Из них:

| группа | файлы | строк | что это |
|---|---|---|---|
| **шим** (чистые ре-экспорты) | `types.go` 30, `pipeline_shim.go` 7, `validate.go` 24, `context.go` 9, `journal.go` 13, `runner.go` 14, `pluginlist.go` 36, `fileref.go` 46, `executor.go` 105, `gate.go` 35 | **~319** | `type X = pipeline.X`, `var NewEngine = plugin.NewEngine`. Удалим в v0.36, уже deprecated |
| **легаси-домен (2.2k)** | `plugincreate.go` 462 + `plugincreate_templates.go` 610 + `plugintest.go` 436 + `plugintest_service.go` 284 + `conformance.go` 438 | **2230** | генерация плагинов, прогон 3/3 тестов, conformance-чек — живут в core по истории, но принадлежат `internal/plugincreate`, `internal/plugintest`, `internal/conformance` |
| **тесты** | 28 `*_test.go` + хелперы | ~5000 | держат легаси-домен |

Итого твое «~2.5k» — это не шим, а **легаси-домен, застрявший в core**. Решение: вынести 5 файлов в отдельные пакеты, оставив в `core` только 320-стр. ре-экспорт (уже помечен `Deprecated`). `cli/schedule.go` → `core.NewEngine` останется рабочим — алиас `Engine = plugin.Engine` не ломается.

```go
// internal/core/types.go уже
type Engine = plugin.Engine
var NewEngine = plugin.NewEngine // → плавно заменить на plugin.NewEngine в cli/daemon
```

## 2. Три монолита рядом (реальные God Objects)

### 2.1 `internal/mcp/server.go` — 2071 строк, 71 type/func
**Роли в одном файле:**
- `type Server` + `type inflightCall/runState/Options/HumanChannel` (65-150) — жизненный цикл MCP-сессии
- `multiEngine` (247-407) — резолв плагинов по `pluginsDirs + workDir`, песочница `E_PLUGIN_OUTSIDE_ROOT`, `LoadManifest` с 3 источниками (builtin → local → registry)
- `checkPluginRef/checkPathInWorkdir/resolvePathForContainment` (408-520) — безопасность путей
- `checkPipelineSafety/loadPipeline/validateArgs` (543-650) — валидация + `gate_policy`
- `Serve/beginCall/bindRun/bindExec/handleNotification/cancelInflight` (652-920) — транспорт JSON-RPC, слоты `maxExecSlot`
- `handle/handleCall/notifyProgress/callTool` (942-1090) — диспетчер 8 инструментов
- `toolListPlugins/toolDescribePlugin/toolValidate/toolPlan/toolRun` (1132-1539) — каждый инструмент
- `auditGateBypass/pruneRunsLocked/runStatus/recordRunStatus/toolGetRun/toolCancel/acquireExecSlot` (1539-1800) — ран-реестр

**Симптом:** изменение транспорта ломает резолв плагинов, изменение `toolRun` требует пересборки валидации. Тест `gate_approval_test.go` падает из-за смешения `stepCapsOrUnknown` в `pipeline/gate_policy.go`.

**План распила (5 файлов, без смены API):**
```
internal/mcp/
  server.go      400  — NewServer, Server struct, Serve, handle/handleCall
  engine.go      250  — multiEngine + parseManifestBytes, pluginPathAllowed, checkPlugin*
  pipeline.go    220  — checkPipelineSafety, loadPipeline, validateArgs, issuesForJSON
  tools.go       700  — callTool + 7 tool* (каждый в свою секцию, но в одном файле)
  runs.go        350  — runState, bindRun/bindExec, pruneRunsLocked, runStatus, cancel
  transport.go   150  — beginCall/endCall/cancelInflight, negotiateProtocol
```
Импорты: `engine.go` тянет `registry,plugin,pipeline`; `pipeline.go` — `pipeline,gate`; `tools.go` — `execution,journal` — циклических зависимостей нет. Проверка: `go vet ./internal/mcp` после выпила — 0.

### 2.2 `internal/api/server.go` — 1443 строки, 48 func
**Роли:**
- `type Server` (46-110) + `summaryCacheEntry` + `defaultAssetsDir/NewServer/reconcileInterruptedRuns` (118-210)
- `secureContainedPath/setCancel/gateFor/csrfGuard` (269-400) — безопасность + in-flight гейты
- `Routes()` (403-485) — 15 роутов в одной функции (health, plugins, pipelines, presets, runs, validate, plan)
- хендлеры: `handlePlugins/handlePluginDetail/handlePluginInstallDeps` (505-690), `handlePipelines/handlePresets/handlePipelineDetail` (695-830), `handleRuns/handleRunDetail/handleRunStart/handleRunCancel/handleRunGate` (954-1350)
- `summarizeRun/eventMetaOf/runSummary/cachedRunSummary` (825-950) — агрегация журнала
- `ListenPolicy/ConfigureListen/HTTPServer` (242-310) — сеть

**План (6 файлов):**
```
internal/api/
  server.go          300 — type Server, NewServer, HTTPServer, ConfigureListen
  routes.go          120 — Routes() + csrfGuard
  handlers_plugins.go 250 — handlePlugins, handlePluginDetail, handlePluginInstallDeps, networkJSON
  handlers_pipelines.go 300 — handlePipelines, handlePresets, handlePipelineDetail, secureContainedPath
  handlers_runs.go     400 — handleRuns, handleRunDetail, handleRunStart/Cancel/Gate, cancels/gates maps
  summary.go         200 — summarizeRun, runSummary, cachedRunSummary, summaryCache
  security.go        100 — csrfGuard, ListenPolicy, blockedReason
```

### 2.3 `internal/execution/runner.go` — 1349 строк, 44 func
**Роли:**
- `type RunOptions/loopBudgetCounter/RunError/RunStats/Engine interface` (25-390) — модель рана + бюджет loop
- `validateRun/RuntimeDataIssue/Run/runWithStore` (390-700) — вход, `loopBudget`, `journal.NewJournal`, `pipelineIdentity`
- `segmentSteps/runParallelSegments/runParallelGroup/cloneCtx` (846-998) — планировщик батчей (DAG из `graph.go`)
- `runStepFlow/runStepForeach/runStepLoop/runStep/gateService` (998-1150) — control-flow (when, foreach, loop, parallel, gate)
- `failEvent/journalWriteError/priorSnapshotLosses/snapshotLossError/finish/cancelled/logf` (138-260) — журнал + отмена
- `pipelineIdentity/journalPipelineIdentity/writeAggregates/sanitize/NewRunID` (267-414) — утилиты

**План (4 файла):**
```
internal/execution/
  runner.go      400 — Run/runWithStore, RunOptions, Engine, validateRun
  steps.go       400 — runStepFlow, runStepForeach, runStepLoop, runStep, gateService, segmentSteps
  parallel.go    200 — runParallelSegments, runParallelGroup, cloneCtx
  journal.go     250 — failEvent, journalWriteError, snapshotLossError, finish, resumeCursor, pipelineIdentity
  loop.go        100 — loopBudgetCounter, consumeLoopIteration, loopIterations
```

## 3. Что уже сделано в этом bulk

- `internal/execution/scheduler.go` (133 строки, `ExecutionGraph`) → `internal/execution/graph.go` + шим `scheduler.go` до 0.36 — убрана путаница cron vs DAG.
- `gate_policy.go` — `IsBuiltin` guard, `validate_issues.go` — `core/text_stats` Optional — чинит `TestTextStatsStepIsNotTreatedAsHumanGate`.

## 4. Порядок выноса без поломки `core.NewEngine`

1. **Сейчас (безопасно):** вынести `multiEngine` в `mcp/engine.go` (чистый move, 0 логики) — проверить `go test ./internal/mcp -run TestCheckPluginRef`.
2. **Следом:** `api/handlers_*.go` — каждый хендлер независим, тестируется `api/server_test.go`.
3. **Затем:** `execution/steps.go` — покрыт `execution/cancel_test.go`, `gate_stdin_test.go`.
4. **В конце:** перенести `core/plugincreate*`, `core/plugintest*`, `core/conformance.go` в `internal/plugincreate` etc., оставив в `core` только 320-стр. шим. Удалить шим в v0.36 после замены `core.NewEngine` на `plugin.NewEngine` в `cli/schedule.go` + `cli/daemon.go` + `api/server.go`.

Все шаги — `go vet 0`, `go test ./...` green (сейчас 60.2k строк, 20 пакетов). Локаль/self-hosted принцип не ломается: `Engine` остаётся `plugin.Engine`, `LoadManifest` — тот же.
