# Образ WEDRA для серверного развёртывания.
#
# Собирается из исходников, а не из релизного бинаря: релиз собирается
# динамически (CGO по умолчанию), и такой бинарь требует glibc той версии, на
# которой собран. Здесь CGO выключен явно, и получается статический бинарь,
# который работает в любом базовом образе.
#
#   docker build -t wedra:local .
#   docker build -t wedra:local --target gui .     # с консолью
#
# Что внутри и почему:
#
#   bubblewrap — НЕ опция, а необходимость. На Linux изоляция внешнего кода
#     это bwrap; без него демон не запустит недоверенный плагин и вернёт
#     sandbox_unavailable. Ставится из apt, потому что в busybox его нет.
#   python3 — интерпретатор для плагинов. Плагины на чистом Python 3 — их
#     65 из 100, и они должны работать в контейнере из коробки.
#   git — плагины могут ставиться из git-репозитория.
#   ca-certificates — HTTPS к реестру и к вебхукам оповещений.
#
# Каталоги вынесены в тома: /data (плагины, пайплайны, журнал ранов) и
# /etc/wedra (файл доверия). Файл доверия НЕ должен быть в образе: он задаёт,
# каким коду можно запускаться, и решать это оператору хоста, а не сборщику
# образа.

# --- сборка -----------------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Зависимости отдельно от исходников: слой с модулями переиспользуется, пока
# меняется код, и сборка не идёт по сети каждый раз.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 — статический бинарь. Проект не использует os/user, поэтому
# чисто-Go реализация ничего не ломает, а libc в образе не нужна.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -ldflags="-s -w -X github.com/wykserdex/wedra/internal/buildinfo.Version=${VERSION}" \
      -o /out/wedra ./cmd/wedra && \
    CGO_ENABLED=0 go build \
      -ldflags="-s -w -X github.com/wykserdex/wedra/internal/buildinfo.Version=${VERSION}" \
      -o /out/tool ./cmd/tool

# --- базовый образ ----------------------------------------------------------
# Базой служит python:…-slim, а не debian:bookworm-slim, и это не случайно:
# в Debian 12 pip внешне управляется (PEP 668), и `pip install` донора плагина
# упал бы с «externally-managed-environment». То есть на «голом» debian
# автоустановка объявленных зависимостей — то, ради чего образ и нужен, — не
# работала бы. В образе на python pip работает штатно, а bubblewrap ставится
# тем же apt.
FROM python:3.14-slim-bookworm AS runtime

# bubblewrap — НЕ опция, а необходимость. На Linux изоляция внешнего кода
# это bwrap; без него демон не запустит недоверенный плагин и вернёт
# sandbox_unavailable. Ставится из apt, потому что в busybox его нет.
# ca-certificates — HTTPS к реестру и к вебхукам оповещений.
# git — плагины могут ставиться из git-репозитория.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      bubblewrap \
      ca-certificates \
      git && \
    rm -rf /var/lib/apt/lists/*

# Непривилегированный пользователь: контейнер не должен работать от root.
# И uid фиксированный, иначе в volume попадают файлы с чужими правами при
# следующем запуске.
RUN groupadd --system --gid 10001 wedra && \
    useradd --system --uid 10001 --gid wedra --home-dir /data --shell /usr/sbin/nologin wedra

COPY --from=build /out/wedra /usr/local/bin/wedra
COPY --from=build /out/tool /usr/local/bin/tool

# Каталоги данных и доверия. Права выданы владельцу: том монтируется поверх, и
# права внутри тома задаёт оператор.
RUN mkdir -p /data/plugins /data/examples /data/var/runs /etc/wedra && \
    chown -R wedra:wedra /data /etc/wedra && \
    chmod 700 /etc/wedra

# Реально поддерживаемая переменная: путь к файлу доверия. Каталоги плагинов,
# пайплайнов и ранов переменными окружения НЕ задаются — таких переменных в
# проекте нет, они передаются флагами (--plugins/--pipelines/--runs-dir), и
# подставлять их в образ флагами же, чтобы настройка не расходилась с
# настоящей.
ENV WEDRA_TRUST_CONFIG=/etc/wedra/wedra-trust.yaml

USER wedra
WORKDIR /data
VOLUME ["/data", "/etc/wedra"]

# По умолчанию — планировщик. Адрес раздачи ссылок одобрения: по умолчанию
# локальный, и для доступа с другой машины его надо переопределить
# (WEDRA_APPROVE_URL), иначе ссылка уйдёт туда, куда человек не достанется.
ENTRYPOINT ["/usr/local/bin/wedra"]
CMD ["daemon", \
     "--plugins=/data/plugins", \
     "--pipelines=/data/examples", \
     "--runs-dir=/data/var/runs"]

# --- образ с консолью ------------------------------------------------------
# Тот же бинарь и те же тома, но входной точкой консоль, и она слушает порт.
FROM runtime AS gui

USER wedra
EXPOSE 8765
ENTRYPOINT ["/usr/local/bin/wedra"]
CMD ["gui", \
     "--plugins=/data/plugins", \
     "--pipelines=/data/examples", \
     "--runs-dir=/data/var/runs", \
     "--listen", "0.0.0.0:8765", "--no-session"]

# ВНИМАНИЕ про --no-session в образе: без сессии /api/* открыт всем, кто
# дотянулся до порта. В образе это осознанно (консоль внутри сети, без
# человека за терминалом), и НЕЛЬЗЯ публиковать такой порт наружу: для
# внешнего доступа нужны --allow-remote и --public-host, а сессию лучше
# оставить включённой. Подробности — в docs/deployment.md.
