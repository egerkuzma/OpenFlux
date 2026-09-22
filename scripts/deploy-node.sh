#!/bin/bash
# Собрать ноду под Linux и отправить её на ноду.
#
#   ./scripts/deploy-node.sh
#
# Скрипт только собирает и отдаёт. Устанавливает и перезапускает — человек у
# панели, кнопкой: установка роняет туннель, и выбирать момент должен тот, кто
# в этот момент им пользуется, а не тот, кто прислал сборку.
#
# Токен берётся из ~/.config/openflux/nodectl-token или из NODECTL_TOKEN.
# В репозитории его нет и быть не должно.
set -euo pipefail

# Адрес панели и токен берутся из ~/.config/openflux/, а не из репозитория:
# и то и другое описывает одну конкретную установку и в общий код не годится.
PANEL_FILE="${NODECTL_PANEL_FILE:-$HOME/.config/openflux/nodectl-panel}"
PANEL="${NODECTL_PANEL:-}"
if [ -z "$PANEL" ] && [ -f "$PANEL_FILE" ]; then
    PANEL=$(tr -d '[:space:]' < "$PANEL_FILE")
fi
if [ -z "$PANEL" ]; then
    echo "нет адреса панели: положите его в $PANEL_FILE" >&2
    echo "  например: echo http://10.0.0.5:8787 > $PANEL_FILE" >&2
    exit 1
fi
TOKEN_FILE="${NODECTL_TOKEN_FILE:-$HOME/.config/openflux/nodectl-token}"
OUT="${TMPDIR:-/tmp}/openflux-linux"

TOKEN="${NODECTL_TOKEN:-}"
if [ -z "$TOKEN" ] && [ -f "$TOKEN_FILE" ]; then
    TOKEN=$(tr -d '[:space:]' < "$TOKEN_FILE")
fi
if [ -z "$TOKEN" ]; then
    echo "нет токена: положите его в $TOKEN_FILE или задайте NODECTL_TOKEN" >&2
    exit 1
fi

cd "$(dirname "$0")/.."

echo "== собираю под Linux =="
GOOS=linux GOARCH=amd64 go build -o "$OUT" .
printf '   %s, %s МБ\n' "$(git rev-parse --short HEAD)" "$(du -m "$OUT" | cut -f1)"

# Проверяем до отправки, а не после: выкатывать сборку, которая не проходит
# собственные тесты, смысла нет.
if [ "${SKIP_TESTS:-}" != "1" ]; then
    echo "== тесты =="
    if ! go test ./... >/tmp/deploy-tests.log 2>&1; then
        echo "   тесты не прошли, не выкатываю:" >&2
        tail -15 /tmp/deploy-tests.log >&2
        exit 1
    fi
    echo "   прошли"
fi

echo "== отправляю на ноду =="
# Ответ приходит одной строкой. Ничего не перезапускается: панель принимает
# файл, проверяет, что это исполняемый файл Linux, и кладёт ждать.
code=$(curl -sS -o /tmp/deploy-reply.txt -w '%{http_code}' \
       --max-time 120 \
       -F "binary=@$OUT" \
       "$PANEL/upload?t=$TOKEN&fmt=text") || {
    echo "   панель не ответила — проверьте, поднят ли туннель" >&2
    exit 1
}
cat /tmp/deploy-reply.txt
[ "$code" = "200" ] || { echo "   код ответа $code" >&2; exit 1; }

echo
echo "Сборка лежит на ноде. Установить и перезапустить — кнопкой в панели:"
echo "   $PANEL/?t=<токен>" 
