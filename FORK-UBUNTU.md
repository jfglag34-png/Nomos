# Ubuntu Server: 6 независимых цепочек

В этой версии добавлена команда `wedra fleet`. Старый `wedra daemon` не менялся. Не запускайте оба планировщика для одних цепочек: это приведёт к двойным запускам.

## Возможности

- До 6 одновременных цепочек по умолчанию (concurrency: 1..64).
- Отдельный cron из пяти полей, timezone и общий таймаут на цепочку.
- Запуск по POST /trigger/<id> с Bearer-токеном.
- success, failure, timeout, cancelled, started, stopped: Telegram и исходящий JSON-вебхук, независимо друг от друга, до 3 попыток доставки.
- Вторая копия fleet с тем же runs_dir блокируется flock.
- Одна и та же цепочка не запускается повторно, пока работает предыдущая; при занятых слотах cron пропускается с записью в journal, HTTP возвращает 409. Не очередь.
- Отдельный каталог запуска, output.log и result.json; таймаут убивает группу процессов.

## Установка

Нужны Ubuntu, systemd, Go 1.26+ для сборки, Python/другие зависимости выбранных плагинов и bubblewrap для недоверенного кода. Архив содержит исходники, а отдельный бинарник собран для Linux amd64 (не ARM).

Из корня этого репозитория:

```bash
sudo apt update
sudo apt install -y bubblewrap python3 python3-venv ca-certificates tzdata
# Установите Go 1.26+ с go.dev, если ещё не установлен.
go build -o wedra ./cmd/wedra
sudo install -m 755 wedra /usr/local/bin/wedra
id wedra >/dev/null 2>&1 || sudo useradd --system --create-home --home-dir /var/lib/wedra --shell /usr/sbin/nologin wedra
sudo install -d -m 750 -o wedra -g wedra /var/lib/wedra/pipelines /var/lib/wedra/plugins /var/lib/wedra/fleet-runs
sudo install -d -m 750 /etc/wedra
sudo install -m 640 -o root -g wedra deploy/fleet/fleet.yaml /etc/wedra/fleet.yaml
sudo install -m 600 deploy/fleet/wedra-fleet.env.example /etc/wedra/fleet.env
sudo install -m 644 deploy/fleet/wedra-fleet.service /etc/systemd/system/
```

1. В `/etc/wedra/fleet.yaml` замените пути на **свои реальные** pipeline YAML. Шесть parser-N.yaml — placeholders, не готовые парсеры. Внутренний `schedule` пайплайна этому режиму не нужен.
2. Установите необходимые плагины в `/var/lib/wedra/plugins` штатными средствами WEDRA, от пользователя wedra. Проверьте каждый pipeline отдельно из `/var/lib/wedra`. Не отключайте проверку доверия ради запуска чужого кода. См. SECURITY.md и docs/deployment.md.
3. В `/etc/wedra/fleet.env` задайте токен Telegram-бота, chat_id и URL webhook. Неиспользуемый канал удалите или оставьте пустым, не оставляйте примеры значений.
4. Для WEDRA_TRIGGER_TOKEN используйте результат `openssl rand -hex 32`.
5. `auto_approve: false` — безопасное значение по умолчанию. Для полностью автоматических цепочек удалите ручные гейты или осознанно установите `auto_approve: true`: это передаёт `--yes`. Интерактивного человека у службы нет; без решения гейт может завершиться ошибкой или дойти до таймаута.

```bash
cd /var/lib/wedra
sudo -u wedra /usr/local/bin/wedra fleet --config=/etc/wedra/fleet.yaml --check
sudo systemctl daemon-reload
sudo systemctl enable --now wedra-fleet
sudo journalctl -u wedra-fleet -f
```

Изменения конфига требуют `sudo systemctl restart wedra-fleet`.

## Ручной запуск и здоровье

На сервере, подставив токен из env:

```bash
curl -X POST -H 'Idempotency-Key: unique-request-001' -H 'Authorization: Bearer YOUR_TOKEN' http://127.0.0.1:8770/trigger/parser-1
curl -H 'Authorization: Bearer YOUR_TOKEN' http://127.0.0.1:8770/health
```

202 = запуск принят (не завершён), 200 = повтор ключа без нового запуска, 400 = нет/невалиден Idempotency-Key, 409 = уже работает/все слоты заняты, 401 = неверный токен, 404 = неизвестная цепочка. Тело запроса не исполняется и не задаёт команды. Запускать можно только цепочки из конфига.

Снаружи используйте TLS reverse proxy с ограничением частоты запросов или SSH-туннель. Не открывайте HTTP-порт напрямую в Интернет. Health защищён тем же токеном. Пути в YAML относительно WorkingDirectory, а не каталога конфига.

Пример исходящего JSON:

```json
{"event":"success","job":"parser-1","detail":"completed","at":"2026-10-19T12:00:00Z"}
```

## Что именно считается «встал»

В этой реализации зависание определяется **общим runtime-таймаутом**, не прогрессом парсера. Ставьте timeout больше нормального времени работы. Успех означает нулевой exit code штатного `wedra pipeline run`, а не бизнес-проверку количества найденных записей. Бесконечный парсер лучше разбить на конечные циклы по расписанию. При SIGTERM работающие цепочки отменяются, systemd перезапускает службу.

Если убит сам процесс fleet, отключилось питание, сеть или весь сервер — он не может сам прислать алерт. Для этого необходим независимый монитор на другой машине: проверка `/health` через HTTPS/туннель, алерт через 1–2 минуты недоступности. Секрет проверяющего храните только у доверенного монитора: он также разрешает запуск цепочек. Автоматический внешний монитор здесь не развёрнут.

Ограничения: нет persistent-очереди, catch-up пропущенных cron после простоя, гарантированной доставки алертов/outbox, автоматической очистки истории. POST требует Idempotency-Key. Повтор того же ключа не запускает цепочку даже после завершения и рестарта; новый ключ означает новый запрос. См. docs/webhook-idempotency.md. Потерянные после 3 попыток уведомления остаются только в journal. Настройте ротацию/удаление старых run-каталогов; output.log может содержать чувствительные данные. Проверка --check валидирует fleet-конфиг и наличие файлов, но не семантику pipeline и доступность плагинов.

## Проверка и форк

`go test ./...` и `go test -race ./internal/cli ./internal/cron` выполнены в среде разработки. Реальная доставка в Telegram/ваш webhook и работа ваших парсеров не проверялись: нужны ваши настройки. Ubuntu/systemd на целевом сервере ещё не проверены.

Для собственного GitHub-форка: создайте fork через GitHub, распакуйте исходники, замените origin на URL своего форка и закоммитьте изменения. Путь Go-модуля оставлен upstream, чтобы не ломать импорты; переименовать его можно отдельно. Лицензия upstream сохранена.

## MCP-конструктор и повтор каждые N минут

Добавлен `compose_pipeline`: агент строит до 256 шагов, группирует параллельные этапы и сохраняет проверенные pipeline/fleet YAML. См. [docs/mcp-compose.md](docs/mcp-compose.md). В fleet можно задать `every: 7m` вместо cron. Первый запуск после старта, дальше каждые 7 минут; перекрывающиеся прогоны пропускаются. Агент не включает расписание без проверки оператором и не включает автоодобрение.

## Защита webhook от дублей

POST /trigger теперь требует Idempotency-Key. Квитанции сохраняются с fsync, повтор после рестарта не исполняется. Это at-most-once admission, не exactly-once: при падении между записью и запуском возможен потерянный запуск. Контракт, ограничения и тесты: [docs/webhook-idempotency.md](docs/webhook-idempotency.md).
