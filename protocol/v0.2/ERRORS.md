# ERRORS v0.2 — коды ошибок для LLM-агентов

Коды — стабильный публичный контракт. Тексты `message` можно менять, коды нельзя.
`ok:false` в `validate` — это нормальный результат, не исключение: агент читает
`issues[0].code`, берет `hint`/`fix.candidates`, исправляет YAML и повторяет.

## Формат Issue

```json
{
  "code": "E_TYPE_MISMATCH",
  "severity": "error",
  "step": "triage",
  "port": "email",
  "path": "steps.triage.email",
  "message": "шаг triage, порт email: тип string несовместим с выходом ...",
  "hint": "нужен тип ...",
  "fix": {"op": "bind", "target": "steps.triage.email", "candidates": ["steps.syntax.email"]}
}
```

- `severity`: `error` блокирует запуск, `warning` — нет.
- `path`: `pipeline.*`, `pipeline.steps.<id>.*`, `steps.*`, `input.*`, `input.<arr>[<i>]`.
- `fix.op`: `bind` — перепривязать порт; `set` — выставить значение;
  `declare` — объявить недостающее (secrets).
- `fix.candidates` отсортирован, пуст — если подсказать нечего.

## Ошибки валидации (pre-run)

| Код | Когда | Подсказка |
|---|---|---|
| E_FORMAT_VERSION | `format_version` не 0.1/0.2 | `fix.candidates: ["0.1","0.2"]` |
| E_CYCLE | цикл в DAG | разорвите зависимость |
| E_FOREACH_PATH | `pipeline.foreach` не с `input.`/`steps.` | `fix.candidates: input.*` |
| E_FOREACH_SHAPE | `steps.*` не вида `steps.<id>.<field>` | формат в hint |
| E_FOREACH_NOT_FOUND | массив/шаг для foreach не найден | `fix.candidates: input.*` |
| E_STEP_ID_EMPTY | пустой `id` | дайте id |
| E_STEP_ID_DUP | дубль `id` | уникальные id |
| E_WHEN_OP | неизвестный `when.op` | `fix.candidates: [truthy,exists,...]` |
| E_WHEN_PATH | `when.path` не с `input.`/`steps.` | — |
| E_WHEN_FORWARD_REF | `when` читает шаг ниже по списку | только шаги выше |
| E_STEP_FOREACH_PATH | `steps.<id>.foreach` плохой путь | — |
| E_STEP_FOREACH_SHAPE | `steps.<id>.foreach` не `steps.<id>.<field>` | — |
| E_STEP_FOREACH_NOT_FOUND | массив для step-foreach не найден | `fix.candidates: input.*` |
| E_FOREACH_COMBO | `foreach` + `after_foreach`/`parallel_group` | уберите одно |
| E_FOREACH_ITEM_NAME | `foreach_item` не простое имя | буквы/цифры/`_` |
| E_GATE_FOREACH | `human_gate` с `foreach` | гейт — один набор полей |
| E_GATE_PARALLEL | `human_gate` в `parallel_group` | вынесите гейт |
| E_GATE_BIND | `human_gate` с `bind` | данные — через `form` |
| E_ON_ERROR | `on_error` не stop/skip/retry | `fix.candidates` |
| E_RETRY_ATTEMPTS | `retry.attempts < 1` | `>=1` |
| E_ON_REJECT | `on_reject` не stop/continue | `fix.candidates` |
| E_PLUGIN_LOAD | плагин не загрузился | проверьте путь и `plugin.yaml` |
| E_BIND_UNKNOWN_PORT | `bind` на несуществующий порт | `fix.candidates: порты плагина` |
| E_NETWORK_DENIED | плагин заявил сеть, а сеть не разрешена: `network: deny` или поле не задано | уберите сеть из манифеста или укажите `network: allow` |
| E_NETWORK_NOT_ENFORCEABLE | `network: allow`, но плагин объявил сеть списком `host:port`, а точечный фильтр не реализован | объявите `any_host: true` или уберите сеть |
| E_NETWORK_VALUE | `pipeline.network` не allow/deny | `fix.candidates: [allow,deny]` |
| E_PORT_UNBOUND | обязательный порт без привязки | `fix.candidates: input.* + steps.*` |
| E_PORT_SOURCE | источник не резолвится (`в input нет поля`, `шаг не найден выше`, `плагин не объявляет выход`) | `fix.candidates: input.* + совместимые steps.*` |
| E_TYPE_MISMATCH | тип источника ≠ типу порта | `fix.candidates: совместимые steps.*` |
| E_FORMAT_INPUT | литерал `input` не соответствует `format` | исправьте значение |
| E_FORMAT_MISMATCH | формат источника не покрывает формат порта | `fix.candidates: совместимые steps.*` |
| E_OPTIONAL_REQUIRED | читает из `skip`-able шага, но порт не `optional` | сделайте `optional` |
| E_PARALLEL_SPLIT | шаг в `parallel_group` не один | разбей группу |
| E_FILE_REF_NOT_FOUND | `file_ref` файла нет | верните путь или уберите шаг |
| E_APPROVAL_VALUE | `approval` не human/any | `fix.candidates: [human,any]` |
| E_GATES_VALUE | `pipeline.gates` не human_only/any | `fix.candidates` |
| E_FOREACH_LIMIT | `foreach` разворачивает больше `MaxForeachItems` (10000) элементов | сузьте вход или разбейте шаг |
| E_PARALLEL_LIMIT | в группе больше `MaxConcurrentBranches` (32) шагов | разбейте группу |
| E_RETRY_LIMIT | `retry.attempts` вне 1..10 | `fix.candidates: 1..10` |
| E_TIMEOUT_LIMIT | `timeout` вне 0..30m | `timeout: "5m"`, ноль = без предела |
| E_GATE_ACTIONS | `form.actions` пустой или не подмножество `accept`/`reject` | `actions: [accept, reject]` |
| E_BIND_SOURCE_INVALID | `bind` ссылается не на `input.*`/`steps.*` | `fix.candidates: input.* + steps.*` |
| E_SCHEDULE_CRON | `pipeline.schedule.cron` пустой или не 5 полей | `fix.candidates: [\"0 9 * * *\",\"0 * * * *\"]` |
| E_SCHEDULE_TIMEZONE | `pipeline.schedule.timezone` неизвестен | `fix.candidates: [Europe/Minsk,UTC]` |
| E_SCHEDULE_OVERLAP | `pipeline.schedule.overlap` не skip/queue/allow | `fix.candidates: [skip,queue,allow]` |
| E_SCHEDULE_HUMAN_GATE | `pipeline.schedule.human_gate` не autoApprove/wait | `fix.candidates: [autoApprove,wait]` |
| E_PLUGIN_LOAD | плагин не загрузился: битый `plugin.yaml` **любого** поля или несовместимый `platform_api` | проверьте путь и plugin.yaml |

## Предупреждения (pre-run, не блокируют)

| Код | Когда |
|---|---|
| W_FORMAT_VERSION_MISSING | нет `format_version` |
| W_SECRETS_MISSING_ENV | `secrets:` env не задан |
| W_FOREACH_ITEM_TYPE | `input.<arr>[i]` не тот `item_type` |
| W_FOREACH_ITEM_FORMAT | `input.<arr>[i]` не соответствует `item_format` (плохой элемент уронит только себя) |
| W_GATE_FORM_COLLISION | коллизия basename в `form` |
| W_GATE_FORM_MISSING | `form` поле может отсутствовать |
| W_GATE_FORM_SKIP | `form` читает из `skip`-able шага |
| W_NETWORK_DECLARED | плагин заявил сеть (аудит — журнал) |
| W_PORT_OPTIONAL_UNBOUND | `optional` порт без привязки |
| W_PORT_OPTIONAL_SOURCE | `optional` порт с битым источником |
| W_PORT_EMPTY_SOURCE | значение входа задано, но пустое | введите значение или отвяжите порт |
| W_SECRETS_UNUSED | `pipeline.secrets` никто не просит |
| W_SECRETS_UNDECLARED | плагину нужен ключ — объявите в `pipeline.secrets` |
| W_PARALLEL_SINGLE | `parallel_group` из одного шага |
| W_FILE_REF_ROOT | `file_ref` найден от корня, но не от плагина |
| W_PARALLEL_SINGLE | `parallel_group` у одного шага | уберите группу |
| W_FILE_REF_ROOT | `file_ref` ведёт в корень, не в рабочую папку | сузьте путь |
| W_FILESYSTEM_HOST_PATH | шаг или плагин заявил доступ к пути хоста, а не рабочей папки | `filesystem: workspace` и относительный путь |

## Ошибки рантайма (журнал: `step_failed` / `run_failed` → `code`)

| Код | Когда | Судьба элемента/рана |
|---|---|---|
| contract_input | вход шага не собрался (нет пути, тип/формат) | элемент: `skip→ok`, иначе `aborted`; ран живёт |
| contract_output | плагин не вернул обязательное поле или соврал типом/форматом | так же, как `contract_input` |
| platform:\<code\> | платформенная ошибка (`timeout`, `crash`, `spawn_failed`, `protocol_violation`, `platform:<code плагина>`) | ран остановлен |
| when_error | `when` не вычислился | ран остановлен |
| foreach_path | `foreach` путь не найден / не массив | ран остановлен |
| secrets_missing | нет env из `pipeline.secrets` | ран не стартует |
| network_denied | сеть не разрешена (`network: deny` или поле не задано), а плагин её заявил | ран не стартует |
| network_not_enforceable | `network: allow`, но объявление сети неисполнимо (список `host:port` без `any_host`) | ран не стартует |
| network_policy | `pipeline.network` имеет недопустимое значение | ран не стартует |
| validation_failed | pre-run validation не пройдена | ран не стартует |
| run_error | прочее (резолв, resume, параллельная группа) | ран остановлен |
| snapshot_lost | `context.json` не записан (снапшот > 16 МБ, диск/права): в журнале `snapshot_lost` c `reason` и `bytes` | работа идёт до конца, но терминальное событие — `run_failed` со `snapshot_losses`, не `run_end`; `--resume` восстановит не всё |
| journal_write | потеряны события журнала (диск полон, запись не прошла) | терминальное событие записано, ошибка surfaces после него |
| cancelled | отмена: Ctrl+C, `POST /api/runs/<id>/cancel`, MCP `cancel_run` | `run_cancelled`, затем snapshot, процесс плагина убит, retry нет; `--resume` продолжит |

`step_skipped` с `reason:on_error` несёт исходный `code` плагина (`bad_syntax`, ...).
`gate_decision` несёт `source`: `terminal` / `gui` / `auto_yes`.

## Коды MCP / HTTP (ответ инструмента или API, не журнал)

| Код | Где | Когда |
|---|---|---|
| E_PLUGIN_OUTSIDE_ROOT | MCP | ссылка на плагин вне `--plugins` / `--workdir` |
| E_FILE_REF_OUTSIDE_ROOT | MCP | `file_ref` ведёт за пределы `--workdir` |
| E_FILE_REF_UNCHECKED | MCP | `file_ref` не удалось проверить, путь не подтверждён — шаг не исполняется |
| E_RUN_BUSY | MCP, HTTP 409 | уже идёт ран (один за раз) |
| E_RUN_DONE | MCP `cancel_run`, HTTP 409 | отмена уже завершённого рана |
| E_NO_HUMAN_CHANNEL | MCP `run_pipeline` | в пайплайне есть гейт, а консоли человека нет (`wedra mcp --no-gui`); отказ до старта |
| E_GATE_REQUIRED | MCP `run_pipeline` | первый опасный шаг (сеть, запись на диск, чтение секретов — по capabilities плагина) идёт без `core/human_gate` перед собой; отказ до старта. В `Data`: `step`, `plugin` |
| E_NO_GATE_UI | рантайм | гейт в режиме без UI (MCP без консоли): ран остановлен, а не вечное ожидание |
| E_SESSION_REQUIRED | HTTP 401 | любой запрос к `/api/*` без cookie сессии человека (не только мутация) |
| E_SESSION_CODE_INVALID | HTTP 401, HTTP 429 | неверный / истёкший / уже использованный одноразовый код входа; 429 — после нескольких неудач |
| E_HOST_NOT_ALLOWED | HTTP 403 | `Host` запроса не в allow-list сервера (например страница с чужого домена, чей DNS указывает на 127.0.0.1) |
| E_AGENT_EXEC_DENIED | MCP `exec_plugin` | запуск плагина агентом без явной политики (`--allow-agent-exec`); по умолчанию отказ |
| E_AGENT_PLUGIN_UNTRUSTED | MCP `exec_plugin` | плагин, написанный агентом, а политика требует доверия; недоверенный требует `--allow-untrusted-plugins`, иначе изолятор |
| E_AGENT_EXEC_BUSY | MCP `exec_plugin` | одновременных запусков уже 4 (предел); слот берётся после проверки политики, поэтому отказ означает реальную занятость, а не запрет |
| E_AGENT_EXEC_AUDIT | MCP `exec_plugin` | не удалось записать строку аудита; отказ до запуска (fail-closed), потому что неоплаченный запуск недопустим |
| E_AGENT_EXEC_CAPABILITIES | MCP `exec_plugin` | плагин объявил права (сеть, запись на диск, чтение секретов) либо манифест не прочитан; инструмент идёт мимо пайплайна, а значит мимо `human_gate`, ждать некого. Такой плагин запускается только через `run_pipeline` с `core/human_gate` перед опасным шагом; обхода нет, правило проверяет ядро |
| E_BAD_REQUEST | HTTP 400 | тело запроса не разобрано, поле отсутствует или значение не прошло проверку: недопустимое имя плагина, файл вне белого списка редактора |
| E_METHOD_NOT_ALLOWED | HTTP 405 | метод не подходит этому пути: витрина только `GET`, установка и валидация и тесты только `POST` |
| E_PLUGIN_NOT_FOUND | HTTP 404 | плагина нет ни в одной базе каталога (plugins, official, community, agent-plugins) |
| E_EXISTS | HTTP 409 | каталог плагина уже есть; создание ничего не перезаписало |
| E_CREATE_FAILED | HTTP 422 | генератор не создал рабочий плагин (та же ошибка, что у `wedra plugin create`) |
| E_READ_FAILED | HTTP 500 | файлы плагина не прочитались: каталог пуст или недоступен |
| E_WRITE_FAILED | HTTP 500 | запись файла плагина не удалась. Правка идёт по файлам по очереди, поэтому часть могла записаться: правку повторяют целиком, а не догадываются |
| E_TEST_TIMEOUT | HTTP 504 | тесты плагина не уложились в отведённое время. Результат НЕ получен — это не «нулевой успех», и клиент обязан трактовать так же |

HTTP `POST /api/run` на невалидном пайплайне возвращает `400 {ok:false, issues[]}`
(коды из разделов выше), каталог рана не создаётся.

## Human-only гейты (модель угроз — честно)

- `approval: human` на шаге и `pipeline.gates: human_only` запрещают авто-аппрув
  (`--yes` не одобряет, ждёт человека). Для раннов из MCP авто-аппрув выключен всегда.
- Все `/api/*` (кроме `/api/health` и обмена кода `POST /api/session`) требуют
  cookie сессии человека: журналы, входы и выходы шагов, плагины, пайплайны и
  статусы гейта тоже. Cookie — токен с TTL в памяти сервера, а не секрет;
  добывается обменом ОДНОРАЗОВОГО кода, который `wedra gui` печатает в терминал
  человека (или `wedra mcp` открывает в браузере сам, когда гейт ждёт).
  В журнал пишется только хэш `session`. Без cookie — 401 `E_SESSION_REQUIRED`.
  Агент через MCP не имеет инструмента одобрения; `get_run`
  в статусе `waiting_human` говорит «попросите пользователя одобрить в окне wedra».
- `Host` запроса сверяется с allow-list (`127.0.0.1`, `localhost`, `[::1]` и
  явно заданные `--public-host`): чужой `Host` → 403 `E_HOST_NOT_ALLOWED`.
  Это защита от DNS-rebinding; `X-Forwarded-Host` не читается никогда,
  `X-Forwarded-Proto` — только с `--trusted-proxy`. Origin сравнивается с тем,
  что сервер объявляет сам, а не со значением из того же запроса.
- Это защита от того, что агент **случайно или по инструкции** одобрит сам себя
  через доступные ему инструменты. От злонамеренного процесса того же пользователя ОС
  (чтение памяти, правка файлов) она не защищает — такая формулировка честнее,
  чем «security boundary».

## `run_pipeline`: опасный шаг требует гейта (`E_GATE_REQUIRED`)

«У агента нет инструмента одобрения» — половина контракта. Вторая половина: агент
не может **обойти** одобрение, выкинув гейт из пайплайна. Раньше этого не было:
`run_pipeline` проверял только «если гейт есть, нужен ли канал к человеку», и
пайплайн без гейта уходил в исполнение целиком.

**Как определяется опасный шаг.** По capabilities плагина, то есть по `permissions`
его манифеста:

| Право | Опасный? |
|---|---|
| `permissions.network` (любая запись) | да — идёт в сеть |
| `permissions.filesystem: workspace \| write \| readwrite` | да — пишет на диск |
| `permissions.filesystem: read \| none` (и незаданное поле) | нет |
| `permissions.secrets` (непустой список) | да — читает секреты (значения приходят в env) |
| манифест не прочитан (битый `plugin.yaml`) | да — незнание прав не доказательство их отсутствия |

`core/*` (встроенные модули) прав не заявляют и опасными не считаются.
`filesystem: read` в список не входит намеренно: чтение не входит в перечень
«сеть / запись на диск / чтение секретов». Это граница правила, а не заявление о
безопасности чтения.

**Правило.** Решает ПЕРВЫЙ опасный шаг: перед ним обязан стоять `core/human_gate`.
Гейт после опасного шага не считается — человек увидит результат, а не намерение.
Опасных шагов нет — гейт не нужен: пайплайн вроде «посчитать слова» или
«разобрать CSV» агент запускает без гейта.

Отказ — RPC-ошибка с `Data.code = "E_GATE_REQUIRED"` и `Data.step`/`Data.plugin`,
до старта рана (каталог рана не создаётся). Текст ответа называет конкретные права
и говорит, что делать: поставить `core/human_gate` (шаг нельзя ставить в
`parallel_group` и нельзя вешать на него `foreach` — это отдельные коды).

**Чего правило не делает** (важно, чтобы не переоценить):

- Оно не измеряет поведение кода. `permissions` — декларация, а не песочница:
  плагин, заявивший `filesystem: none`, всё равно пишет куда угодно правами
  пользователя. Правило ловит заявленное намерение. Против плагина, который врёт
  в манифесте, работает `sandbox: untrusted` + изолятор ОС (см. SECURITY.md).
- Оно не разбирает `when` и `foreach`. Шаг под `when` может и не выполниться, но
  статически это неизвестно, поэтому он считается выполняющимся: лишний гейт
  безопаснее пропущенного.
- Оно не касается `exec_plugin` (см. следующий раздел) и HTTP `POST /api/run`:
  там решение человека и так предшествует запуску, а MCP — единственная
  поверхность, где инициатором может быть агент.

**Операторский обход: `--allow-unapproved-runs`.** Флаг `wedra mcp` снимает
требование для локального доверенного использования. По умолчанию обхода нет.
Обход **не тихий**:

- при старте сервер печатает предупреждение в stderr (лог MCP-процесса);
- каждый обойдённый ран пишет строку в append-only журнал
  `<runs-dir>/gate-bypass.jsonl` с `fsync` **до** запуска:

```json
{"ts":"2026-09-29T09:12:44.881Z","event":"agent_run_gate_bypassed",
 "source":"operator_flag","flag":"--allow-unapproved-runs","pipeline":"mail_blast",
 "step":"send","plugin":"mailer","capabilities":"сеть, чтение секретов","gate_after":false}
```

- если строка не записалась — ран **не стартует**, отказ `E_GATE_REQUIRED`
  (неоплаченный запуск недопустим, как и в аудите `exec_plugin`).

Файл `gate-bypass.jsonl` не участвует в `get_run`: это журнал безопасности,
а не события рана.

## `exec_plugin`: исполнение плагина агентом

Агент может запустить плагин напрямую, минуя пайплайн и гейты. Это отдельная
поверхность с явной политикой, а не «ещё один инструмент».

**Гейт не запрашивается.** У `exec_plugin` нет шага, где человек мог бы
отказать: агент звонит, политика решает. Доверенным считается только плагин,
подтверждённый ядром (`id` + sha256 содержимого в allow-list); плагин, написанный
агентом, доверенным не бывает в принципе. Такой плагин идёт в изолятор только
при `--allow-untrusted-plugins`; без него — `E_AGENT_PLUGIN_UNTRUSTED`.
По умолчанию выключено всё: без `--allow-agent-exec` — `E_AGENT_EXEC_DENIED`.

**Аудит обязателен: намерение пишется ДО запуска.** На один запуск приходится
две строки в `<runs-dir>/agent-exec.jsonl` — намерение и результат, связанные
общим `exec_id`:

```json
{"ts":"2026-09-28T16:04:20.100Z","event":"agent_plugin_exec_intent","exec_id":"9f2c…",
 "source":"agent_auto","plugin":"text_stats","plugin_id":"text_stats","untrusted":false,
 "timeout":60}
{"ts":"2026-09-28T16:04:20.220Z","event":"agent_plugin_exec","exec_id":"9f2c…",
 "source":"agent_auto","plugin":"text_stats","plugin_id":"text_stats","untrusted":false,
 "exit_code":0,"err_code":"","duration":"120ms"}
```

Границы здесь несимметричны, и их надо различать:

- **Не записалось намерение** → `E_AGENT_EXEC_AUDIT`, и плагин **не запускается
  вовсе**. Это настоящий запрет.
- **Не записался результат** → `E_AGENT_EXEC_AUDIT`, агент не получает
  результат, но плагин уже отработал. Намерение с тем же `exec_id` в журнале
  остаётся.

Почему намерение идёт первым, а не вторым: при записи после запуска падение
процесса посреди исполнения не оставляет **ничего** — код отработал, след
пропал. Намерение до запуска закрывает и этот случай. Файл принудительно
сбрасывается на диск (`fsync`) именно поэтому: иначе буфер ушёл бы в никуда
вместе с процессом.

Читателю журнала это даёт простую проверку: `..._intent` без пары
`agent_plugin_exec` означает, что процесс упал или был убит; `agent_plugin_exec`
без намерения быть не должно — это признак дырки в журнале.

Обе фазы проверяются тестами: `TestAgentExecAuditFailurePreventsExecution`
(запрет работает) и `TestAgentExecAuditRecordsBothAroundRealExecution`
(порядок и связка по `exec_id`).

Файл append-only и не участвует в `get_run`: `agent-exec.jsonl` — журнал
безопасности, а не события рана. Просматривать его нужно отдельно.

**Предел 4 параллельных запусков.** При исчерпании — `E_AGENT_EXEC_BUSY`,
а не ожидание в очереди: очередь означала бы, что агент может висеть
на инструменте дольше, чем он готов ждать.
