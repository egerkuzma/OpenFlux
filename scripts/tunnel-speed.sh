#!/bin/bash
# Throughput through the tunnel, measured two ways on purpose.
#
# A single stream and several parallel ones answer different questions. One
# stream is what a speed test reports and what reordering hurts: frames spread
# across links arrive out of order, TCP reads that as loss, and the congestion
# window stays clamped. Parallel streams share the same path but each keeps its
# own window, so they aggregate past a per-flow limit.
#
# So if the parallel total is much higher than the single stream, the tunnel is
# not short of capacity — something is holding one flow back, and with a bonded
# channel the first suspect is reordering across links.
#
#   ./tunnel-speed.sh [megabytes] [runs]

set -u
MB="${1:-8}"
RUNS="${2:-3}"
BYTES=$((MB * 1000000))
URL="https://speed.cloudflare.com/__down?bytes=$BYTES"

speed_of() {   # prints bytes/sec for one download, one value per line
    # The newline matters: without it, parallel appends to the same file run
    # together into a single absurd number.
    curl -s --max-time 120 -o /dev/null -w '%{speed_download}\n' "$URL" 2>/dev/null
}

human() {
    python3 -c "
b=float('${1:-0}')
print(f'{b*8/1e6:6.2f} Мбит/с  ({b/1e6:5.2f} МБ/с)')"
}

echo "== цель: $MB МБ за проход, $RUNS прохода =="

echo "-- один поток --"
single=()
for i in $(seq 1 "$RUNS"); do
    s=$(speed_of)
    single+=("$s")
    echo "   проход $i: $(human "$s")"
done

echo "-- четыре потока одновременно --"
parallel=()
for i in $(seq 1 "$RUNS"); do
    tmp=$(mktemp)
    for _ in 1 2 3 4; do speed_of >> "$tmp" & done
    wait
    total=$(python3 -c "
vals=[float(x) for x in open('$tmp').read().split() if x.strip()]
# Four downloads are expected; anything else means the results were mangled.
if len(vals) != 4:
    raise SystemExit(f'ожидалось 4 значения, получено {len(vals)}')
print(sum(vals))")
    rm -f "$tmp"
    parallel+=("$total")
    echo "   проход $i: $(human "$total")"
done

python3 - "${single[@]}" -- "${parallel[@]}" <<'PY'
import sys, statistics
args = sys.argv[1:]
cut = args.index('--')
single = [float(x) for x in args[:cut]]
par = [float(x) for x in args[cut+1:]]
ms, mp = statistics.median(single), statistics.median(par)
print()
print(f"  медиана, один поток:      {ms*8/1e6:6.2f} Мбит/с")
print(f"  медиана, четыре потока:   {mp*8/1e6:6.2f} Мбит/с")
if ms > 0:
    ratio = mp/ms
    print(f"  отношение:                {ratio:.1f}x")
    if ratio >= 2.0:
        print("  Одиночный поток не выбирает всю ёмкость. На канале с большой")
        print("  задержкой это обычное дело: одно TCP-соединение не успевает")
        print("  раскрыть окно, а несколько делят путь между собой.")
        print("  Тревожно это лишь в сравнении: если одиночный поток заметно")
        print("  медленнее, чем на одном документе, значит кадры переставляются")
        print("  между каналами — а при передаче по одному каналу за раз такого")
        print("  быть не должно.")
    else:
        print("  Параллельные потоки не дают прироста — упирается сама ёмкость")
        print("  канала, а не поведение TCP.")
PY
