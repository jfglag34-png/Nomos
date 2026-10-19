# Развёртывание на сервере

Два способа поставить WEDRA на машину без человека за терминалом: контейнер
и юнит systemd. Оба решают одну задачу — **чтобы планировщик работал после
перезагрузки, а гейт можно было одобрить без доступа к консоли**.

## Что нужно знать до развёртывания

**Изоляция на Linux — это bubblewrap.** Без `bwrap` в PATH демон не запустит
недоверенный плагин и вернёт `sandbox_unavailable`. Поэтому в образе
`bubblewrap` стоит обязательно, а в юните нужно доустановить его на хосте.

**Файл доверия — это политика, а не настройка.** `wedra-trust.yaml` решает,
какому коду можно запускаться, и читается он СЛЕДСТВЕННО:

1. `--trust-config=…`, если такой флаг есть у команды;
2. `WEDRA_TRUST_CONFIG`;
3. файл рядом с бинарником.

Файл из рабочего каталога **игнорируется намеренно** — иначе клон репозитория
с чужим `wedra-trust.yaml` молча расширял бы доверие. Пустой файл означает
fail-closed: внешний код не запускается вовсе.

**Каталоги задаются флагами, а не переменными окружения.** `--plugins=`,
`--pipelines=`, `--runs-dir=`. Переменных вида `WEDRA_PLUGINS_DIR` в проекте
нет, и в образе они бы просто не подействовали.

## Docker

```bash
docker build -t wedra:local .
docker build -t wedra:local --target gui .      # с консолью
```

Тома: `/data` (плагины, пайплайны, журнал ранов) и `/etc/wedra` (файл
доверия).

```bash
docker volume create wedra-data
docker run -d --name wedra \
    -v wedra-data:/data \
    -v /etc/wedra:/etc/wedra:ro \
    -p 127.0.0.1:8766:8766 \
    --restart unless-stopped \
    wedra:local
```

По умолчанию образ запускает **планировщик**. Консоль — отдельный образ:

```bash
docker run -d --name wedra-gui \
    -v wedra-data:/data \
    -v /etc/wedra:/etc/wedra:ro \
    -p 127.0.0.1:8765:8765 \
    --restart unless-stopped \
    wedra:local gui --listen 0.0.0.0:8765 --no-session
```

### Осторожно с `--no-session`

Без сессии всё под `/api/*` открыто тому, кто дотянулся до порта. В образе это
оправдано (консоль внутри своей сети, человека за терминалом нет), но **публиковать
такой порт наружу нельзя**. Для внешнего доступа:

- `--allow-remote` вместе с `--public-host=имя[:порт]` — иначе запросы
  отвергаются все;
- сессию лучше оставить включённой: одноразовый код печатается в журнал
  контейнера (`docker logs wedra-gui`).

### Ссылки одобрения в контейнере

Демон печатает подписанную ссылку в журнал:

```
docker logs wedra 2>&1 | grep 'откройте'
```

Ссылка указывает на `WEDRA_APPROVE_URL` (по умолчанию `http://127.0.0.1:8766`).
Если человек открывает её с другой машины, этот адрес должен быть достижим
извне:

```bash
docker run -d --name wedra \
    -e WEDRA_APPROVE_URL=https://wedra.example.com \
    -e WEDRA_APPROVE_LISTEN=0.0.0.0:8766 \
    ...
```

Ссылка подписана ключом из `<runs-dir>/approve.key` (в томе `/data`), живёт час
и работает один раз.

## systemd

Файлы: `deploy/systemd/wedra.service` (планировщик) и
`deploy/systemd/wedra-gui.service` (консоль).

```bash
sudo useradd --system --home-dir /var/lib/wedra --shell /usr/sbin/nologin wedra
sudo mkdir -p /var/lib/wedra/{plugins,pipelines,var/runs} /etc/wedra
sudo chown -R wedra:wedra /var/lib/wedra
sudo cp deploy/systemd/wedra.service deploy/systemd/wedra-gui.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now wedra wedra-gui
```

Юниты ожидают готовую развёртку: бинарь в `/usr/local/bin/wedra`, данные в
`/var/lib/wedra`, файл доверия в `/etc/wedra/wedra-trust.yaml`.

Переменные окружения — в `/etc/wedra/wedra.env` (образец:
`deploy/systemd/wedra.env.example`); подключается в юните строкой
`EnvironmentFile=-/etc/wedra/wedra.env`.

### Что ужестоено в юнитах

- `NoNewPrivileges` — запрет setuid-поднятия. **Пользовательским пространствам
  bubblewrap он не мешает**: unprivileged userns это отдельный механизм.
- `ProtectSystem=full` + `ReadWritePaths=/var/lib/wedra` — система read-only,
  писать можно только в данные.
- `ProtectHome=read-only` — домашний каталог на чтение.
- `PrivateTmp` — свой `/tmp`.
- Сетевых фильтров **нет намеренно**: песочница и вебхуки ходят в сеть по
  разным адресам, и жёсткий список сломал бы одно из двух.

### Что стоит добавить оператору

- `MemoryMax=` и `CPUQuota=` — демон запускает плагины, и без потолка один
  плагин может съесть хост.
- `Restart=on-failure` уже стоит; `StartLimitIntervalSec`/`StartLimitBurst`
  настраиваются по вкусу.

## Что проверить после установки

```bash
wedra doctor                 # что сломано в окружении
wedra check                  # полный гейт проекта
systemctl status wedra       # планировщик жив
journalctl -u wedra -n 50    # что он делает
```

И отдельно — **что демон действительно тикает**: после старта он должен создать
каталог состояния `<runs-dir>/.state/`. Если его нет, демон не нашёл ни одного
пайплайна с `schedule:` — проверьте `--pipelines=` и сам YAML.
