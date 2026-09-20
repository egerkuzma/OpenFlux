#!/bin/bash
# Continuous availability probe for the tunnel, plus a report over what it
# collected.
#
# The failure this was built to catch used to be terminal: traffic stopped and
# never came back. So the useful measurement is not "is it up" but "how often
# does it break, and for how long does it stay broken" — a recovery that takes
# a second is a different world from one that never happens.
#
#   ./tunnel-probe.sh run [target] [port]     probe every second until stopped
#   ./tunnel-probe.sh stop                    stop a probe started earlier
#   ./tunnel-probe.sh report                  summarise what has been collected
#
# Defaults probe 1.1.1.1:443 — a plain TCP connect that has to traverse the
# tunnel, without depending on DNS, so a DNS problem cannot be mistaken for a
# tunnel outage.

set -u
SAMPLES="${TUNNEL_PROBE_LOG:-$HOME/Library/Logs/OpenFlux/probe.csv}"
OFLUX_LOG="$HOME/Library/Logs/OpenFlux/openflux.log"
# The sampling loop runs as a python child, so killing this script by name
# leaves the loop orphaned and still writing. Record the child's pid and stop
# that.
PIDFILE="${SAMPLES%.csv}.pid"

mkdir -p "$(dirname "$SAMPLES")"

case "${1:-report}" in
run)
    TARGET="${2:-1.1.1.1}"
    PORT="${3:-443}"
    echo "Probing $TARGET:$PORT every second. Samples -> $SAMPLES"
    echo "Stop with: $0 stop     Report with: $0 report"
    python3 - "$TARGET" "$PORT" "$SAMPLES" <<'PY' &
import socket, sys, time, datetime
target, port, path = sys.argv[1], int(sys.argv[2]), sys.argv[3]
with open(path, "a", buffering=1) as f:
    while True:
        began = time.time()
        s = socket.socket(); s.settimeout(3)
        t0 = time.perf_counter()
        try:
            s.connect((target, port))
            ms = (time.perf_counter() - t0) * 1000
            f.write(f"{datetime.datetime.now().isoformat(timespec='seconds')},ok,{ms:.0f}\n")
        except Exception as e:
            f.write(f"{datetime.datetime.now().isoformat(timespec='seconds')},fail,{type(e).__name__}\n")
        finally:
            s.close()
        # Keep a steady one-second cadence regardless of how long the attempt
        # took, so each sample covers the same amount of wall clock.
        time.sleep(max(0.0, 1.0 - (time.time() - began)))
PY
    child=$!
    echo "$child" > "$PIDFILE"
    # Ctrl+C must take the sampling loop with it, not just this wrapper.
    trap 'kill "$child" 2>/dev/null; rm -f "$PIDFILE"; exit 0' INT TERM
    wait "$child"
    rm -f "$PIDFILE"
    ;;

stop)
    if [ -f "$PIDFILE" ] && kill "$(cat "$PIDFILE")" 2>/dev/null; then
        rm -f "$PIDFILE"
        echo "Пробник остановлен."
    else
        # Fall back to whatever still holds the samples file open: a probe from
        # an earlier run may have outlived its pid file.
        holder="$(lsof -t "$SAMPLES" 2>/dev/null | head -1)"
        if [ -n "$holder" ] && kill "$holder" 2>/dev/null; then
            rm -f "$PIDFILE"
            echo "Пробник остановлен (найден по открытому файлу)."
        else
            echo "Работающий пробник не найден."
        fi
    fi
    ;;

report)
    [ -f "$SAMPLES" ] || { echo "Нет данных: $SAMPLES. Сначала запустите '$0 run'."; exit 1; }
    python3 - "$SAMPLES" "$OFLUX_LOG" <<'PY'
import sys, datetime, statistics, os

samples, oflux = sys.argv[1], sys.argv[2]
rows = []
for line in open(samples):
    parts = line.strip().split(",")
    if len(parts) < 2:
        continue
    try:
        when = datetime.datetime.fromisoformat(parts[0])
    except ValueError:
        continue
    rows.append((when, parts[1] == "ok", parts[2] if len(parts) > 2 else ""))

if not rows:
    print("Данных нет."); sys.exit(0)

ok = sum(1 for _, good, _ in rows if good)
total = len(rows)
span = rows[-1][0] - rows[0][0]

# Group consecutive failures into outages. A single failed sample is still an
# outage of about a second — brief, but real.
outages, start, last = [], None, None
for when, good, _ in rows:
    if not good:
        if start is None:
            start = when
        last = when
    elif start is not None:
        outages.append((start, (last - start).total_seconds() + 1))
        start = None
if start is not None:
    outages.append((start, (last - start).total_seconds() + 1))

print(f"  период наблюдения: {span}  ({total} проб раз в секунду)")
print(f"  доступность:       {ok/total*100:.3f}%  ({total-ok} неудачных проб)")

lat = [float(v) for _, good, v in rows if good and v.replace('.','',1).isdigit()]
if lat:
    print(f"  задержка соединения: медиана {statistics.median(lat):.0f} мс, "
          f"максимум {max(lat):.0f} мс")

if not outages:
    print("  перерывов не зафиксировано")
else:
    lens = [d for _, d in outages]
    print(f"  перерывов: {len(outages)}  "
          f"(самый долгий {max(lens):.0f} с, медиана {statistics.median(lens):.0f} с)")
    longest = sorted(outages, key=lambda o: -o[1])[:5]
    print("  самые длинные:")
    for when, d in longest:
        print(f"    {when:%Y-%m-%d %H:%M:%S}  —  {d:.0f} с")
    # The point of the whole exercise: nothing here should be open-ended.
    if max(lens) > 120:
        print("  ВНИМАНИЕ: перерыв дольше двух минут — похоже на зависание, а не на переподключение")

if os.path.exists(oflux):
    text = open(oflux, errors="ignore").read()
    print()
    print(f"  из лога клиента за текущую сессию:")
    # These come from the ordinary log, so they are there without --debug.
    for label, needle in (
        ("полных обрывов канала", "Transport disconnected"),
        ("восстановлений", "Transport reconnected"),
    ):
        print(f"    {label}: {text.count(needle)}")

    # Link-count changes are how a bond's individual document failures show
    # up at all: while one link survives, the transport never reports itself
    # down, so this is the only always-on evidence that documents died.
    changes = [l for l in text.splitlines() if "Bonded links:" in l]
    if changes:
        print(f"    изменений числа живых документов: {len(changes) - 1}")
        print(f"    последнее состояние: {changes[-1].split('Bonded links:')[-1].strip()}")
        worst = None
        for l in changes:
            try:
                n = int(l.split("Bonded links:")[-1].strip().split()[0])
            except (ValueError, IndexError):
                continue
            if worst is None or n < worst:
                worst = n
        if worst is not None:
            print(f"    минимум живых документов за сессию: {worst}")

    # Optional detail, only present when the profile has debug logging on.
    deeper = text.count("[M-DOCS] Read error")
    if deeper:
        print(f"    (debug) ошибок чтения из документа: {deeper}")
        print(f"    (debug) переподключений к документу: {text.count('connectToDoc attempt')}")
PY
    ;;
*)
    echo "Использование: $0 run [цель] [порт] | $0 stop | $0 report"
    exit 1
    ;;
esac
