#!/usr/bin/env python3
"""Измеряет DNS через туннель: долю ответов, распределение задержек, причины отказов.

Бьёт в тот же путь, которым ходит браузер: UDP-запрос на резолвер, который
клиент перехватывает и переиздаёт как DNS-over-TCP через туннель.

  ./scripts/dns-bench.py 192.168.1.35

Фазы идут от лёгкой к тяжёлой, чтобы было видно, где именно начинается срыв:
одиночные запросы дают базовую задержку, залпы растущего размера — точку, в
которой канал перестаёт справляться. Имена все разные: повтор одного имени
измерял бы кэш резолвера, а не туннель.
"""
import random, socket, statistics, string, sys, time
from collections import Counter

RESOLVER = sys.argv[1] if len(sys.argv) > 1 else "192.168.1.35"
PORT = 53

# Каждое имя уникально. Повторное имя резолвер отдаёт из кэша за миллисекунду,
# и замер тогда меряет его память, а не туннель: первый вариант этого стенда
# показывал p50=1мс и 100% успеха там, где браузер еле шевелился.
#
# Уникальность делается случайной меткой перед настоящим доменом. Ответ будет
# NXDOMAIN, и это правильный ответ: резолвер обязан сходить за ним к
# вышестоящему серверу, то есть пройти весь путь целиком. Нас интересует, что
# путь пройден и за сколько, а не что имя существует.
DOMAINS = """google.com cloudflare.com github.com wikipedia.org yandex.ru
mail.ru vk.com apple.com microsoft.com amazon.com netflix.com reddit.com
stackoverflow.com mozilla.org debian.org kernel.org python.org golang.org
docker.com gitlab.com bitbucket.org nginx.org postgresql.org redis.io
ubuntu.com archlinux.org freebsd.org openbsd.org rust-lang.org nodejs.org
telegram.org signal.org protonmail.com duckduckgo.com bing.com yahoo.com
twitch.tv spotify.com soundcloud.com vimeo.com flickr.com imgur.com
dropbox.com box.com slack.com discord.com zoom.us atlassian.com
jetbrains.com sublimetext.com vscode.dev npmjs.com pypi.org rubygems.org
crates.io packagist.org maven.org gradle.org cmake.org gnu.org""".split()


def parse_a(data):
    """Достаёт первый A-адрес из ответа. Нужен, чтобы проверять не только факт
    ответа, но и его содержимое: внутреннее имя, отданное чужим адресом, — это
    отравленный кэш, а не рабочий резолв."""
    if len(data) < 12:
        return None
    qd, an = data[4] << 8 | data[5], data[6] << 8 | data[7]
    if an == 0:
        return None
    i = 12
    for _ in range(qd):  # пропускаем вопрос
        while i < len(data) and data[i]:
            i += data[i] + 1
        i += 5
    for _ in range(an):
        if i >= len(data):
            return None
        if data[i] & 0xC0 == 0xC0:
            i += 2
        else:
            while i < len(data) and data[i]:
                i += data[i] + 1
            i += 1
        if i + 10 > len(data):
            return None
        rtype = data[i] << 8 | data[i + 1]
        rdlen = data[i + 8] << 8 | data[i + 9]
        i += 10
        if rtype == 1 and rdlen == 4:
            return ".".join(str(b) for b in data[i:i + 4])
        i += rdlen
    return None


def internal_zone():
    """Внутренние имена читаются из ~/.config/openflux/dns-bench-internal,
    по строке "имя адрес". В репозитории их нет: они описывают одну сеть."""
    import os
    path = os.path.expanduser("~/.config/openflux/dns-bench-internal")
    out = []
    try:
        for line in open(path):
            line = line.split("#")[0].strip()
            if line and len(line.split()) == 2:
                out.append(tuple(line.split()))
    except OSError:
        pass
    return out


def unique_name():
    label = "".join(random.choices(string.ascii_lowercase + string.digits, k=12))
    return f"{label}.{random.choice(DOMAINS)}"


def query(name, timeout=5.0):
    """Один UDP-запрос A-записи. Возвращает (задержка_мс, причина_отказа|None)."""
    tid = random.randint(0, 0xFFFF)
    header = bytes([tid >> 8, tid & 0xFF, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0])
    q = b"".join(bytes([len(p)]) + p.encode() for p in name.split(".")) + b"\x00"
    packet = header + q + bytes([0, 1, 0, 1])

    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(timeout)
    started = time.time()
    try:
        s.sendto(packet, (RESOLVER, PORT))
        while True:
            data, _ = s.recvfrom(4096)
            if len(data) >= 2 and (data[0] << 8 | data[1]) == tid:
                break
        elapsed = (time.time() - started) * 1000
        rcode = data[3] & 0x0F
        # 0 — есть запись, 3 — имени нет. И то и другое означает, что запрос
        # дошёл до вышестоящего сервера и ответ вернулся: путь пройден.
        if rcode not in (0, 3):
            return elapsed, f"rcode={rcode}", None
        return elapsed, None, parse_a(data)
    except socket.timeout:
        return (time.time() - started) * 1000, "таймаут", None
    except OSError as e:
        return (time.time() - started) * 1000, f"сокет: {e.errno}", None
    finally:
        s.close()


def run(names, concurrency):
    """Задаёт names с заданной одновременностью. Возвращает список (t, мс, отказ)."""
    import threading
    out, lock = [], threading.Lock()
    start = time.time()

    def worker(chunk):
        for n in chunk:
            ms, err, _ = query(n)
            with lock:
                out.append((time.time() - start, ms, err))

    chunks = [names[i::concurrency] for i in range(concurrency)]
    threads = [threading.Thread(target=worker, args=(c,)) for c in chunks if c]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    return out, time.time() - start


def report(label, results, wall):
    ok = [ms for _, ms, err in results if err is None]
    bad = Counter(err for _, _, err in results if err is not None)
    total = len(results)
    rate = 100.0 * len(ok) / total if total else 0.0

    line = f"  {label:<22} {len(ok):>4}/{total:<4} ({rate:5.1f}%)"
    if ok:
        ok.sort()
        p = lambda q: ok[min(len(ok) - 1, int(len(ok) * q))]
        line += f"  p50 {statistics.median(ok):6.0f}мс  p90 {p(0.9):6.0f}мс  p99 {p(0.99):6.0f}мс"
        line += f"  {total / wall:5.1f} зпр/с"
    print(line)
    if bad:
        print(f"  {'':<22} отказы: " + ", ".join(f"{k}×{v}" for k, v in bad.most_common()))
    return results


def main():
    print(f"=== DNS через {RESOLVER}:53 ===")
    print("  (имена все разные — меряем туннель, а не кэш резолвера)\n")

    every = []

    # Разогрев не считаем: первый запрос поднимает соединение.
    run([unique_name() for _ in range(3)], 1)

    print("  фаза                 успех            задержки                        темп")
    for label, count, conc in [
        ("одиночные", 20, 1),
        ("залп 4", 20, 4),
        ("залп 16", 32, 16),
        ("залп 32", 48, 32),
        ("залп 64", 64, 64),
    ]:
        names = [unique_name() for _ in range(count)]
        res, wall = run(names, conc)
        every += res
        report(label, res, wall)
        time.sleep(1.5)

    # Долгий ровный прогон: ловит периодические провалы, например в такт
    # обновлению сессий mailru (каждые ~50 с).
    print()
    names = [unique_name() for _ in range(180)]
    res, wall = run(names, 4)
    every += res
    report("ровно 4 потока", res, wall)

    fails = [(t, err) for t, _, err in res if err]
    if fails:
        print("\n  отказы по секундам от начала фазы:")
        buckets = Counter(int(t // 10) * 10 for t, _ in fails)
        for sec in sorted(buckets):
            print(f"    {sec:>4}–{sec+10:<4}с  {'#' * buckets[sec]} ({buckets[sec]})")
        print("    (пики каждые ~50с указывают на обновление сессий mailru)")

    zone = internal_zone()
    if zone:
        print("\n  внутренняя зона (знает только резолвер за нодой):")
        wrong = 0
        for name, want in zone:
            ms, err, got = query(name, timeout=5)
            if err:
                print(f"    {name:<24} ОТКАЗ: {err}")
                wrong += 1
            elif got != want:
                print(f"    {name:<24} {got} — ожидался {want}  ← подмена или чужой резолвер")
                wrong += 1
            else:
                print(f"    {name:<24} {got:<16} {ms:5.0f}мс")
        if wrong:
            print(f"    {wrong} из {len(zone)} внутренних имён не разрешились верно")
    else:
        print("\n  внутренняя зона не задана: положите имена в")
        print("    ~/.config/openflux/dns-bench-internal  (строки вида \"nas.lan 192.168.1.100\")")

    ok = sum(1 for _, _, e in every if e is None)
    print(f"\n  ИТОГО: {ok}/{len(every)} ({100.0*ok/len(every):.1f}%)")


if __name__ == "__main__":
    main()
