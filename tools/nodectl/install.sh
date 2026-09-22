#!/bin/bash
# Установка панели управления нодой. Запускать на ноде от root:
#
#   sudo bash install.sh <токен>
#
# Ничего не ломает на полпути: сначала готовит всё новое, проверяет, и только
# потом перезапускает ноду. Юнит openflux не правится — рядом кладётся
# дополнение к нему, и откат сводится к удалению одного файла.
set -euo pipefail

TOKEN="${1:-}"
PANEL_USER="${2:-$(id -un)}"
ADDR="${3:-127.0.0.1:8787}"
BIN_SRC="${4:-/tmp/nodectl}"

[ "$(id -u)" -eq 0 ] || { echo "нужен root: sudo bash $0 <токен>" >&2; exit 1; }
[ ${#TOKEN} -ge 16 ] || { echo "токен должен быть не короче 16 символов" >&2; exit 1; }
[ -f "$BIN_SRC" ] || { echo "нет файла панели: $BIN_SRC" >&2; exit 1; }
id "$PANEL_USER" >/dev/null 2>&1 || { echo "нет пользователя $PANEL_USER" >&2; exit 1; }

echo "== 1. параметры ноды переезжают в файл =="
# Берём то, чем нода запущена прямо сейчас, чтобы первое состояние панели
# совпало с действующим и ничего не изменилось в момент установки.
# systemctl печатает строку запуска вместе со своими полями — argv, коды
# возврата, время старта. Обрезаем ровно по первому из них: без этого в файл
# параметров уехал бы служебный хвост и нода не поднялась бы.
CURRENT=$(systemctl show openflux -p ExecStart --value \
          | sed -n 's/.*\/usr\/local\/bin\/openflux \(.*\) ; ignore_errors.*/\1/p' | head -1)
if [ -z "$CURRENT" ]; then
    CURRENT="--role=exit --mode=l4 --transport=mailru --url-file=/etc/openflux/url.txt --encryption-key-file=/etc/openflux/secret.txt"
    echo "   не разобрал нынешнюю строку запуска, беру значения по умолчанию"
fi
case "$CURRENT" in
    *";"*|"") echo "строку запуска разобрать не вышло, останавливаюсь: [$CURRENT]" >&2; exit 1 ;;
esac
install -d -m 755 /etc/openflux
if [ ! -f /etc/openflux/flags ]; then
    printf 'OPENFLUX_FLAGS=%s\n' "$CURRENT" > /etc/openflux/flags
fi
touch /etc/openflux/url.txt
chown "$PANEL_USER" /etc/openflux/flags /etc/openflux/url.txt
chmod 644 /etc/openflux/flags /etc/openflux/url.txt
echo "   $(cat /etc/openflux/flags)"

echo "== 2. дополнение к юниту =="
install -d -m 755 /etc/systemd/system/openflux.service.d
cat > /etc/systemd/system/openflux.service.d/nodectl.conf <<'EOF'
# Параметры берутся из файла, чтобы панель могла их менять, не трогая юнит.
# Пустой ExecStart обязателен: без него systemd добавит вторую команду к
# существующей, а не заменит её.
[Service]
EnvironmentFile=/etc/openflux/flags
ExecStart=
ExecStart=/bin/sh -c 'exec /usr/local/bin/openflux $OPENFLUX_FLAGS'
EOF
echo "   положено в /etc/systemd/system/openflux.service.d/nodectl.conf"

echo "== 3. права ровно на четыре команды =="
cat > /etc/sudoers.d/nodectl <<EOF
$PANEL_USER ALL=(root) NOPASSWD: /usr/bin/systemctl start openflux, /usr/bin/systemctl stop openflux, /usr/bin/systemctl restart openflux, /usr/bin/install -m755 -o root -g root /var/lib/nodectl/openflux.new /usr/local/bin/openflux
EOF
chmod 440 /etc/sudoers.d/nodectl
visudo -c >/dev/null
echo "   проверено visudo"

echo "== 4. панель =="
install -d -m 755 -o "$PANEL_USER" /var/lib/nodectl
install -m 755 "$BIN_SRC" /usr/local/bin/nodectl
cat > /etc/systemd/system/nodectl.service <<EOF
[Unit]
Description=Панель управления нодой OpenFlux
After=network-online.target

[Service]
User=$PANEL_USER
Environment=NODECTL_ADDR=$ADDR
Environment=NODECTL_TOKEN=$TOKEN
ExecStart=/usr/local/bin/nodectl
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
chmod 600 /etc/systemd/system/nodectl.service   # в нём токен

echo "== 5. запуск =="
systemctl daemon-reload
systemctl enable --now nodectl
sleep 1
systemctl is-active --quiet nodectl || { echo "панель не поднялась:"; journalctl -u nodectl -n 20 --no-pager; exit 1; }

# Ноду перезапускаем последней: до этого момента всё было обратимо и она
# работала по-старому.
systemctl restart openflux
sleep 2
systemctl is-active --quiet openflux || { echo "ВНИМАНИЕ: нода не поднялась"; journalctl -u openflux -n 20 --no-pager; exit 1; }

echo
echo "готово. панель: http://$ADDR/?t=$TOKEN"
echo
echo "откат:"
echo "  systemctl disable --now nodectl"
echo "  rm -f /etc/systemd/system/nodectl.service /etc/sudoers.d/nodectl /usr/local/bin/nodectl"
echo "  rm -rf /var/lib/nodectl /etc/systemd/system/openflux.service.d/nodectl.conf"
echo "  systemctl daemon-reload && systemctl restart openflux"
