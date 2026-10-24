#!/usr/bin/env bash
# Установка и обновление Invoice Keeper на VPS (Ubuntu/Debian).
#
# Запускать на сервере из каталога репозитория:
#   sudo ./deploy/deploy.sh install   # первый раз
#   git pull && sudo ./deploy/deploy.sh update
#   ./deploy/deploy.sh status | logs | db
#
# install создаёт пользователя, каталог /opt/invoice-keeper и systemd-юнит;
# update пересобирает бинарник и перезапускает сервис, не трогая .env и базу.
set -euo pipefail

APP=invoice-keeper
APP_DIR=/opt/invoice-keeper
RUN_USER=invoicekeeper
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

require_root() {
    [[ $EUID -eq 0 ]] || { echo "Запустите через sudo: sudo $0 $*"; exit 1; }
}

# Go нужен только на сервере-сборке; при not found ставим официальный пакет.
ensure_go() {
    command -v go >/dev/null 2>&1 && return
    echo "Go не найден — устанавливаем golang"
    apt-get update -qq
    apt-get install -y -qq golang-go
}

build_binary() {
    ensure_go
    cd "$SRC_DIR"
    echo "Сборка $APP_DIR/$APP"
    # modernc.org/sqlite не требует cgo — бинарник самодостаточен.
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$APP" ./cmd/server
}

do_install() {
    require_root install
    command -v apt-get >/dev/null || { echo "Поддерживаются Ubuntu/Debian"; exit 1; }

    id -u "$RUN_USER" >/dev/null 2>&1 || useradd --system --home "$APP_DIR" --shell /usr/sbin/nologin "$RUN_USER"
    install -d -m 750 -o "$RUN_USER" -g "$RUN_USER" "$APP_DIR" "$APP_DIR/data"

    build_binary
    install -m 755 -o "$RUN_USER" -g "$RUN_USER" "$SRC_DIR/$APP" "$APP_DIR/$APP"
    install -m 644 -o "$RUN_USER" -g "$RUN_USER" .env.example "$APP_DIR/.env.example"
    [[ -f "$APP_DIR/.env" ]] || install -m 640 -o "$RUN_USER" -g "$RUN_USER" .env.example "$APP_DIR/.env"

    install -m 644 "deploy/systemd/$APP.service" "/etc/systemd/system/$APP.service"
    systemctl daemon-reload
    systemctl enable --now "$APP"

    cat <<EOF

Установлено. Дальше:
  1. Заполните настройки:   nano $APP_DIR/.env     # ADMIN_PASSWORD и JWT_SECRET обязательно
  2. Перезапустите:         systemctl restart $APP
  3. Проверьте статус:      $0 status
  4. Положите nginx-конфиг: deploy/nginx/$APP.conf (см. комментарии в файле)

EOF
}

do_update() {
    require_root update
    require_installed

    build_binary
    # Бинарник подменяем остановленным сервисом: файл в памяти перезаписывать нельзя.
    systemctl stop "$APP"
    install -m 755 -o "$RUN_USER" -g "$RUN_USER" "$SRC_DIR/$APP" "$APP_DIR/$APP"
    install -m 644 "deploy/systemd/$APP.service" "/etc/systemd/system/$APP.service"
    systemctl daemon-reload
    systemctl start "$APP"
    systemctl --no-pager --lines=0 status "$APP" || true
    echo "Обновлено. .env и база в $APP_DIR/data не изменялись."
}

require_installed() {
    [[ -f "/etc/systemd/system/$APP.service" ]] || { echo "Сервис не установлен — сначала: sudo $0 install"; exit 1; }
}

case "${1:-}" in
    install) do_install ;;
    update)  do_update ;;
    status)  require_installed; systemctl status "$APP" --no-pager ;;
    logs)    require_installed; journalctl -u "$APP" -f ;;
    restart) require_root restart; systemctl restart "$APP" ;;
    db)
        # Прямая работа с базой: $0 db "SELECT * FROM users"
        require_installed
        db_path=$(awk -F= '/^DATABASE_PATH=/{print $2}' "$APP_DIR/.env" | tail -1)
        db_path=${db_path:-data/invoice-keeper.db}
        [[ $db_path = /* ]] || db_path="$APP_DIR/$db_path"
        command -v sqlite3 >/dev/null || { echo "Установите: apt-get install sqlite3"; exit 1; }
        shift
        if [[ $# -eq 0 ]]; then
            sqlite3 "$db_path"
        else
            sqlite3 "$db_path" "$*"
        fi
        ;;
    *)
        sed -n '2,12p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        exit 1
        ;;
esac
