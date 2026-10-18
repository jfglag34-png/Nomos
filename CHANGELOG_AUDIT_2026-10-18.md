# Changelog — аудит 1-2-3 (2026-10-18)

## 567f5ee — doctor / network / trust / format_version
- **doctor**: `internal/doctor/donor.go: donorFromFiles()` сканирует `README*`+`*.py` когда `description` пуст; `pipInstallRe` + фильтры `urlInstallTargetRe`/`vcsPrefixes`/`validPackageName` чтобы `pip install https://...` не стало донором `https`. `doctor.go:160` fallback `donorFromDescription → donorFromFiles`. 13 плагинов (community/webanalyze и др.) теперь `verified`, `unverified 13→0`.
- **doctor тесты**: `doctor_test.go:885,918` обновлены — `readmeonly` ∉ `unverified`, `really_unverified` с `https://` URL остаётся неверифицированным; `HumanOutput` проверяет `unverified`.
- **network**: остаётся `any_host` only (fail-closed), без новых декларативных хостов — `execution/runner.go` warn `network_not_enforceable`.
- **format_version**: `pipeline/validate_issues.go:93` `0.3+` теперь `warn` (совместимость), не `error`.
- **trust Windows**: `api/trust.go` `windows_note`+`example_path`+`how_to_create` с PowerShell подсказкой.

## 2e2f5e4 — вынос plugincreate + conformance из core
- `internal/core/plugincreate.go` (463 строк) + `plugincreate_templates.go` (610) → `internal/plugincreate/` package `plugincreate` + `doc.go`, shim `core/plugincreate_shim.go` (`CreateOptions` alias + `ListTemplates`/`TemplateNames`/`PyLiteralForTest`).
- `internal/core/conformance.go` (439) → `internal/conformance/` + `doc.go`, shim `core/conformance_shim.go`, локальные `Manifest/NewEngine` алиасы вместо `core/types.go`.
- `go vet ./...` ok, `go test ./...` 23 ok (1.8–2.2s).

## d4431aa — вынос plugintest из core (финал step1)
- `internal/core/plugintest.go` (437) + `plugintest_service.go` (284) → `internal/plugintest/` + `doc.go`; локальные `Manifest/Duration/NewEngine` + `truncate=common.Truncate` + `kindOf` без цикла; `EnforceOutput → plugin.EnforceOutput`.
- Тесты `plugintest_test.go`+`service_test.go`+`shipped_test.go` перенесены в новый пакет с `helpers_test.go=requirePython`, `testdata` продублирована (`core/testdata → plugintest/testdata`).
- `core/plugintest_shim.go` алиасы `TestService/PluginTestFile/...` + `RunPluginTests/Ctx`.
- `go test ./...` 23 ok, `core 6s`, `plugintest 1s`.

## Дальше (аудит)
- `execution/scheduler.go → graph.go` rename, `runtime.requires 13/41`, `importer TODO`, scheduler MVP `schedule:` (cron/timezone/queue|allow|catchUp/humanGate+daemon lock) — в работе.
