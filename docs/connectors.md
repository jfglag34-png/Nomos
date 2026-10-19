# Nomos connectors: PostgreSQL, JSON HTTP API, Telegram

Four ordinary versioned Python plugins live in plugins/community. They use the same ports/bind, trust, permissions and journals as other plugins. No new execution bypass or secret storage is added. The four connectors are registered with commit pins in the Nomos registry; upstream plugin pins and authorship remain unchanged.

## Install and validate

From a full Nomos checkout, with Go 1.26+ and Python 3:

```bash
go build -o nomos ./cmd/nomos
# PostgreSQL driver; use a dedicated venv on Ubuntu (PEP 668):
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r plugins/community/postgres_read/requirements.lock
./nomos plugin list
./nomos plugin validate plugins/community/postgres_read
./nomos pipeline validate examples/connectors/postgres_read.yaml
```

The runtime must find the venv interpreter; run CLI/fleet with the venv bin directory first in PATH (systemd: Environment=PATH=/var/lib/wedra/venv/bin:/usr/local/bin:/usr/bin). Copy/install connectors into the service's plugins/community directory, not just the source checkout. Built-in trust follows registered content hashes; modified connector code needs an explicit operator trust decision. Do not disable sandbox/trust checks to make an arbitrary plugin work.

Secrets must be in the process environment or a protected systemd EnvironmentFile, never YAML input/bind, repository files or webhook payloads. NOMOS_ names are explicitly declared in each plugin's permissions.secrets so the runner can pass only the required environment to it. Manifest permissions are declarations, not a host-level network firewall.

## PostgreSQL read

Environment: `NOMOS_POSTGRES_DSN` (e.g. PostgreSQL URI; do not paste a real password in logs). For remote production databases use sslmode=verify-full and a trusted CA. The connector does not silently rewrite TLS policy; the operator controls the DSN. Use a dedicated SELECT-only database role, never the postgres superuser.

Inputs:

- sql: one parameterized statement.
- params: positional JSON array for `%s`, OR named_params: object for `%(name)s`.
- max_rows: 1..10000, default 1000.
- statement_timeout_ms: 1..120000, default 30000; lock_timeout capped at 10000 ms.

Output: columns (array), rows (array of arrays), row_count (driver count), truncated (bool), read_only=true. Decimal/UUID are strings, dates/times ISO strings, bytea is an object with base64. Client encoding is UTF8. Output limit 8 MiB, connect timeout 10 seconds. Use a step-level timeout as an independent upper bound. fetchmany limits returned rows, not necessarily database memory/work: add SQL LIMIT where appropriate.

Read-only mode is enforced by the database transaction, not a fragile SELECT regex. Parameter values are passed separately via psycopg extended protocol; multiple statements are rejected. Read-only is not a replacement for least privilege: functions can have non-database side effects. Error output omits the DSN, query, parameter values and raw server exception; SQLSTATE is included if available.

Example: examples/connectors/postgres_read.yaml.

## PostgreSQL write

Separate plugin `postgres_write`. Requires both DSN and explicit operator `NOMOS_POSTGRES_ALLOW_WRITE=1`. Only a single INSERT, UPDATE or DELETE is accepted (leading comments, WITH/DDL and multi-statements are intentionally unsupported). Use a restricted write role for only intended tables.

Writes are transactional. SQL errors, output serialization errors and oversized RETURNING results roll back the transaction. A RETURNING result exceeding max_rows is an error and rollback, not truncated successful output. A successful write returns read_only=false.

**No automatic database retries.** A connection loss around COMMIT can leave an unknown outcome: errors are non-retryable. A database UNIQUE business key/UPSERT and a deliberate reconciliation strategy are necessary for exactly-once business intent; webhook idempotency alone does not guarantee it. Do not configure blind step retries for writes.

The example examples/connectors/postgres_write.yaml has a real core/human_gate, gates: human_only and approval: human, so --yes cannot approve that example's gate. This is a pipeline control, not a new mandatory per-plugin gate in the core: an operator can author a different chain without it. The plugin's independent default-deny boundary is the write environment opt-in plus DB privileges. Review SQL and permissions before enabling it.

A human-only gate is not unattended automation: fleet has no remote approval UI for these subprocess runs and can fail/time out waiting for input. Run this example interactively. Fully unattended writes require a separately reviewed operator policy/configuration, not an agent silently enabling auto-approval.

## Universal JSON HTTP API

`http_request` provides a reusable connector for REST-like JSON services; it is not a bespoke OAuth integration for every provider.

Operator environment:

| Name | Purpose |
|---|---|
| NOMOS_HTTP_BASE_URL | Required fixed origin/base path, e.g. https://api.example.com/v1 |
| NOMOS_HTTP_BEARER_TOKEN | Optional bearer token |
| NOMOS_HTTP_HEADERS_JSON | Optional object of string headers (API keys, custom auth) |
| NOMOS_HTTP_ALLOW_PRIVATE=1 | Explicit opt-in for private/loopback endpoints |
| NOMOS_HTTP_ALLOW_INSECURE=1 | Explicit opt-in for plain HTTP; otherwise HTTPS only |
| NOMOS_HTTP_ALLOW_WRITE=1 | Explicit opt-in for POST/PUT/PATCH/DELETE |

Caller supplies path, not credentials or an arbitrary origin. Path must stay under the configured base path. URLs/next links cannot leave the origin; redirects are rejected. DNS resolves to validated addresses and the TCP connection is pinned to a checked address while TLS still verifies the original hostname. Proxy environment variables are not used. Private, loopback, link-local and reserved addresses are denied unless the operator opts in. For a genuine host-wide egress guarantee use OS/network controls too; other plugins' declarations are not that firewall.

Input: path, method (default GET), optional JSON object body, timeout_seconds (1..60 per request), retries (0..3, default 2, only GET/HEAD), max_pages (1..20, default 1), next_path (dot path in JSON response, e.g. links.next), optional idempotency_key for the remote API.

GET/HEAD may retry network errors and HTTP 429/502/503/504. Numeric Retry-After is capped at 5 seconds; otherwise bounded exponential delay. Mutating methods are never retried automatically, even with an idempotency key. Headers/token come only from operator environment; response/error logs do not echo credential-bearing request URLs or raw error bodies.

Pagination follows the string next link from next_path; relative links resolve against the current URL. Cycles, cross-origin links and path escapes fail. No arbitrary Link-header/cursor/offset pagination dialect is inferred. max_pages limits the walk; truncated=true means a next link remained at the cap. POST pagination is not supported.

Output: responses (array of {status_code, body}), page_count and truncated. Combined response-body cap 4 MiB, each request body <=4 MiB. Only JSON responses are accepted, compression is rejected. Per-request timeout is not the whole pagination deadline: set a step timeout (e.g. 120s). Examples/connectors/http_api.yaml is a template: configure the actual service URL/schema first.

Not included: OAuth consent/refresh-token flows, automatic provider schema discovery, GraphQL query builder, streaming, file download/upload and service-specific SDKs. Some APIs implement side effects on GET: only use endpoints you trust.

## Telegram as a chain step

Plugin `telegram_send` is separate from the fleet alert outbox. Set NOMOS_TELEGRAM_TOKEN. chat_id is a string input or falls back to NOMOS_TELEGRAM_CHAT_ID.

Provide **exactly one**:

- text: plain text, max 4096 UTF-16 code units.
- document_base64: base64 bytes, decoded 1 byte..5 MiB; optional filename (simple ASCII filename) and caption (<=1024 UTF-16 units).

Documents are inline data: the plugin never reads a caller-selected local file. No parse_mode/HTML rendering. timeout_seconds 1..60, default 30. Fixed Telegram HTTPS endpoint, proxies/redirects disabled, response bound 64 KiB. Output: sent, message_id, chat_id. Never automatically retries a send: ambiguous delivery can otherwise create duplicates. Unlike fleet alerts, ordinary Telegram steps are **not** automatically placed in the fleet notification outbox. Actual bot/chat permissions still need setup and a live end-to-end test.

Example: examples/connectors/telegram_message.yaml. To attach a small report, bind a base64-producing upstream output to document_base64.

## MCP and safety

`compose_pipeline` now accepts network: allow (default deny) to generate and validate network-enabled **drafts**. Plugin-root and file_ref confinement checks remain active. Output includes mcp_network_policy_allowed and, for network-enabled drafts, an execution note. Activated is always false. Existing run_pipeline/exec_plugin network restrictions are unchanged: an operator must review/run these network chains via CLI/fleet. Compose does not install packages, provide credentials or enable writes.

## Tests and CI

```bash
python3 -m unittest discover -s tests/connectors -v
# To include real PostgreSQL tests, point at a dedicated throwaway database only:
NOMOS_TEST_POSTGRES_DSN='postgresql://TEST_ROLE@HOST/THROWAWAY_DB' \
  python3 -m unittest discover -s tests/connectors -v
```

The integration tests create/drop a random test table in that database. Never point them at production. CI has a PostgreSQL 17 service with disposable test-only credentials and runs all 21 Python tests plus plugin contracts. Offline tests use a local HTTP server and mocked Telegram transport; no live bot token is needed. A real local PostgreSQL 17 test run passed, including transaction rollback, read-only enforcement, timeout and SQL-injection-as-literal checks.

Pipeline inputs/results/journals can still contain sensitive business data (query params and successful API/DB responses). Error redaction does not make logs safe to publish. Restrict their filesystem access and retention.
