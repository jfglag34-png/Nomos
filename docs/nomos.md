# Nomos: название, установка и совместимость

Nomos — форк WEDRA (автор исходного проекта wykserdex). Сохранены исходная MIT-лицензия, авторство и история. Новое название обозначает развитие форка, а не переименование чужого авторства.

## Сборка

Нужен Go 1.26+. CI использует Go 1.26.9.

```bash
go build -o nomos ./cmd/nomos
./nomos version
./nomos --help
./nomos demo
```

Новые релизные артефакты называются nomos-linux-amd64, nomos-linux-arm64, nomos-darwin-*, nomos-windows-*.exe, nomos-gui-windows-*.exe. CLI fleet работает только на Linux; остальные команды доступны на поддерживаемых платформах. Workflow не публикует релиз от обычного push: нужен одобренный tag v<VERSION> после всех проверок существующего release pipeline. Версия в VERSION пока унаследована: переименование само по себе не выпуск новой версии.

GitHub Actions: Nomos CI запускает прежние проверки плюс reusable job Nomos reliability с повторными race/аварийными тестами, проверкой MCP, smoke-тестами Nomos и совместимого CLI, проверкой юнитов и тестовым Linux-артефактом. Для ручного запуска — workflow_dispatch у Nomos CI или Nomos reliability. CI-артефакт не равнозначен подписанному релизу.

## Обновление службы на Ubuntu

Сначала сделайте резервную копию данных при остановленной службе. Не удаляйте .durable и .trigger-claims.

```bash
sudo systemctl stop wedra-fleet
sudo install -m 755 nomos /usr/local/bin/nomos
sudo install -m 644 deploy/fleet/nomos-fleet.service /etc/systemd/system/
sudo systemctl disable wedra-fleet
sudo systemctl daemon-reload
sudo systemctl enable --now nomos-fleet
sudo journalctl -u nomos-fleet -f
```

Если старой службы нет — команды stop/disable для неё не нужны. Существующие fleet.env, fleet.yaml, пользователь wedra и каталог /var/lib/wedra сохраняются. Никогда не включайте две службы для одной очереди. Для чистой установки используйте FORK-UBUNTU.md, но ставьте cmd/nomos и nomos-fleet.service.

## Переменные окружения

Все NOMOS_* нормализуются в соответствующие WEDRA_* при старте CLI. При наличии обоих значений NOMOS_* имеет приоритет, даже если оно пустое:

```text
NOMOS_TRIGGER_TOKEN
NOMOS_ALERT_WEBHOOK
NOMOS_TELEGRAM_TOKEN
NOMOS_TELEGRAM_CHAT_ID
NOMOS_TRUST_CONFIG
```

Старые WEDRA_* продолжают работать. Пример новых настроек: deploy/fleet/nomos-fleet.env.example. Значения секретов в репозиторий не добавляются.

## Что намеренно не переименовано

- module github.com/wykserdex/wedra и внутренние import paths: не меняется идентичность модуля и не ломаются импорты одним косметическим коммитом. Каноническая сборка форка — из клона Nomos через ./cmd/nomos; go install старого upstream URL устанавливает upstream, не этот форк.
- cmd/wedra и прежние скрипты: совместимый CLI остаётся.
- WEDRA_* и файловые имена/пути данных, доверия и браузерных настроек: без скрытого сброса состояния или границы безопасности.
- исходные URL, commit-пины и подписи плагинов в registry.yaml, trust seed и copyright: происхождение и доверие не подменяются.
- исторический CHANGELOG, протокол и архивная документация исходного проекта.

Docker собирает бинарник nomos, делает его entrypoint и сохраняет symlink wedra. Пользователь контейнера/пути томов остаются прежними. Название image/tag выбирает оператор; для Nomos используйте docker build -t nomos:local .
