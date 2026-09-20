#!/bin/bash
# Pull a large amount of data through the tunnel and watch for stalls.
#
# Short transfers say almost nothing here: the tunnel moves traffic to another
# document roughly once a minute, and a download that finishes in a second
# usually slips between those moves. A transfer that runs for an hour cannot,
# so this is what tells us whether a long-lived connection — a corporate agent
# pulling policies, say — actually survives them.
#
# Two things matter for the result to mean anything:
#
#   * The data must not compress. The tunnel runs zstd, so /dev/zero would be
#     squeezed to nothing and report a speed that does not exist.
#   * The source must sit behind the exit node, so the bytes really cross the
#     tunnel rather than the local network.
#
#   ./tunnel-stress.sh <гигабайт> <хост>
#
# The host is an ssh destination reachable from behind the exit node; there is
# no default, because a default here would be one operator's machine name.
#
# Ctrl+C stops it and still prints the report.

set -u
GB="${1:-3}"
HOST="${2:-}"
if [ -z "$HOST" ]; then
    echo "укажите хост за выходной нодой: ./tunnel-stress.sh $GB <хост>" >&2
    exit 1
fi
MB=$((GB * 1024))
LOG="$HOME/Library/Logs/OpenFlux/openflux.log"
STALLS="${TMPDIR:-/tmp}/tunnel-stress-stalls.txt"

: > "$STALLS"

echo "== тянем $GB ГБ с $HOST через туннель =="
echo "   при нынешней скорости это примерно $(python3 -c "print(f'{$MB/60:.0f}')") минут"
echo "   прервать можно Ctrl+C, отчёт всё равно будет"
echo

start_epoch=$(date +%s)

# The reader goes in its own file: piping data into `python3 -` would put the
# program and the data on the same stdin, and the program text would win.
READER="${TMPDIR:-/tmp}/tunnel-stress-reader.py"
cat > "$READER" <<'PYREADER'
import sys, time, datetime

want_mb = int(sys.argv[1])
stalls_path = sys.argv[2]
stall_after = 2.0          # пауза дольше этого считается зависанием

got = 0
started = time.time()
last_data = started
last_report = started
stalls = []

try:
    while True:
        chunk = sys.stdin.buffer.read(65536)
        now = time.time()
        if not chunk:
            break

        gap = now - last_data
        # The very first chunk waits for ssh to connect and dd to start, which
        # is not a stall in the tunnel.
        if gap >= stall_after and got > 0:
            stalls.append((datetime.datetime.now() - datetime.timedelta(seconds=gap), gap))
            print(f"   ЗАВИСАНИЕ {gap:.1f} с в {datetime.datetime.now():%H:%M:%S} "
                  f"(получено {got/1e6:.0f} МБ)", flush=True)
        last_data = now
        got += len(chunk)

        if now - last_report >= 30:
            rate = got / (now - started)
            done = got / 1e6
            left = (want_mb - done) / (rate / 1e6) if rate > 0 else 0
            print(f"   {done:7.0f} МБ из {want_mb}  |  {rate*8/1e6:5.2f} Мбит/с  "
                  f"|  осталось ~{left/60:.0f} мин  |  зависаний {len(stalls)}", flush=True)
            last_report = now
except KeyboardInterrupt:
    pass

took = time.time() - started
with open(stalls_path, "w") as f:
    for when, gap in stalls:
        f.write(f"{when:%H:%M:%S},{gap:.1f}\n")

print()
print(f"   получено: {got/1e6:.0f} МБ за {took/60:.1f} мин")
if took > 0:
    print(f"   средняя скорость: {got*8/took/1e6:.2f} Мбит/с")
print(f"   зависаний дольше {stall_after:.0f} с: {len(stalls)}")
if stalls:
    longest = max(g for _, g in stalls)
    total = sum(g for _, g in stalls)
    print(f"   самое долгое: {longest:.1f} с, суммарно простояли {total:.0f} с "
          f"({total/took*100:.1f}% времени)")
if got < want_mb * 1e6 * 0.99:
    print("   ПЕРЕДАЧА НЕ ЗАВЕРШИЛАСЬ — получено меньше запрошенного")
PYREADER

ssh -o BatchMode=yes -o ConnectTimeout=20 -o ServerAliveInterval=15 "$HOST" \
    "dd if=/dev/urandom bs=1M count=$MB status=none" 2>/dev/null \
  | python3 -u "$READER" "$MB" "$STALLS"

end_epoch=$(date +%s)

# The point of the exercise: line the stalls up against what the tunnel was
# doing. A stall that lands on a link switch points at the handover; one that
# does not means the cause is elsewhere.
echo
echo "== что делал туннель за это время =="
python3 - "$LOG" "$start_epoch" "$end_epoch" "$STALLS" <<'PY'
import sys, re, datetime, os

log, t0, t1, stalls_path = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), sys.argv[4]
begin = datetime.datetime.fromtimestamp(t0).time()
end = datetime.datetime.fromtimestamp(t1).time()

switches, closures = [], []
for line in open(log, errors="ignore"):
    m = re.match(r"\d{4}/\d{2}/\d{2} (\d{2}:\d{2}:\d{2})\s+(.*)", line.strip())
    if not m:
        continue
    t = datetime.datetime.strptime(m.group(1), "%H:%M:%S").time()
    if not (begin <= t <= end):
        continue
    if "traffic moved to link" in m.group(2):
        switches.append(t)
    elif "connection lost" in m.group(2):
        closures.append(t)

print(f"   переключений канала: {len(switches)}")
print(f"   закрытий сессий:     {len(closures)}")

stalls = []
if os.path.exists(stalls_path):
    for line in open(stalls_path):
        parts = line.strip().split(",")
        if len(parts) == 2:
            stalls.append((datetime.datetime.strptime(parts[0], "%H:%M:%S").time(), float(parts[1])))

if not stalls:
    print("   зависаний не было — передача шла ровно, переключения её не задевали")
elif not switches:
    print("   зависания были, но переключений не происходило — причина не в них")
else:
    def secs(t):
        return t.hour * 3600 + t.minute * 60 + t.second
    near = 0
    for when, _ in stalls:
        if min(abs(secs(when) - secs(s)) for s in switches) <= 3:
            near += 1
    print(f"   зависаний рядом с переключением (±3 с): {near} из {len(stalls)}")
    if near >= len(stalls) * 0.5:
        print("   ВЫВОД: зависания совпадают с переключениями — виноват переход между каналами")
    else:
        print("   ВЫВОД: зависания в основном НЕ совпадают с переключениями — искать надо в другом месте")
PY
