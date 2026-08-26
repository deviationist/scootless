#!/usr/bin/env python3
"""scoot-watch — watch the Entur mobility feed and identify YOUR scooter.

Two modes:

  --interactive : cache the map, poll continuously, and when you type `start`
                  (you just unlocked) or `end` (you just finished), infer WHICH
                  scooter is yours from the vanish / reappearance that lines up
                  with the moment you told it. This is the "confirm by
                  disappearance" trick, driven by you.

  (default)     : passively log every vanish / reappearance with a Google Maps
                  link, optionally filtered to specific scooter numbers.

Why GraphQL not GBFS: GBFS free_bike_status is ~2 MB and un-gzipped; the GraphQL
endpoint honours gzip and lets you pick fields, so the whole Oslo Ryde fleet is
~130 KB on the wire — small enough to poll every few seconds politely.

Resolution note: the feed itself refreshes about every 30 s, so that is the real
floor on timing a vanish. Polling faster pins when Entur's copy flips, not the
physical unlock instant — which is why several unlocks in the same 30 s window
can be indistinguishable (an anonymity window). The correlation logic below
waits out that window before deciding.

Examples:
  ./scoot-watch.py --interactive --near 59.9300,10.7660 --radius 1500
  ./scoot-watch.py --track 370341,373389 --interval 3
"""
import argparse
import gzip
import json
import sys
import threading
import time
import urllib.error
import urllib.request
from collections import deque

ENDPOINT = "https://api.entur.io/mobility/v2/graphql"
OPERATORS = {"ryde": "YRY:Operator:Ryde", "voi": "YVO:Operator:voi",
             "bolt": "YBO:Operator:bolt", "dott": "YDT:Operator:dott"}


def query(lat, lon, radius, operator_id, client_name):
    q = ('{ vehicles(lat:%f, lon:%f, range:%d, operators:["%s"], count:20000)'
         '{ id lat lon currentRangeMeters } }' % (lat, lon, radius, operator_id))
    req = urllib.request.Request(
        ENDPOINT, data=json.dumps({"query": q}).encode(),
        headers={"Content-Type": "application/json", "ET-Client-Name": client_name,
                 "Accept-Encoding": "gzip"})
    with urllib.request.urlopen(req, timeout=25) as r:
        raw = r.read()
        if r.headers.get("Content-Encoding") == "gzip":
            raw = gzip.decompress(raw)
    d = json.loads(raw)
    if d.get("errors"):
        raise RuntimeError(d["errors"][0].get("message", "graphql error"))
    out = {}
    for v in d["data"]["vehicles"] or []:
        out[v["id"].split(":")[-1][2:8]] = (v["lat"], v["lon"], v.get("currentRangeMeters") or 0)
    return out


def maps(p):
    return "https://www.google.com/maps?q=%f,%f" % (p[0], p[1])


def loc(p):
    """Raw lat,lon followed by a Google Maps link."""
    return "%.6f,%.6f  %s" % (p[0], p[1], maps(p))


def stamp(t=None):
    return time.strftime("%H:%M:%S", time.localtime(t))


class Watcher(threading.Thread):
    """Background poller: maintains current set and a rolling event log."""

    def __init__(self, lat, lon, radius, op_id, client_name, interval, track):
        super().__init__(daemon=True)
        self.lat, self.lon, self.radius = lat, lon, radius
        self.op_id, self.client_name, self.interval = op_id, client_name, interval
        self.track = track
        self.lock = threading.Lock()
        self.current = {}
        self.events = deque(maxlen=4000)   # (time, kind, number, position)
        self.stop = False
        self.ready = threading.Event()
        self.quiet = False

    def _poll(self):
        full = query(self.lat, self.lon, self.radius, self.op_id, self.client_name)
        if self.track:
            full = {n: p for n, p in full.items() if n in self.track}
        return full

    def run(self):
        while not self.stop:
            try:
                cur = self._poll()
            except urllib.error.HTTPError as e:
                if e.code == 429:
                    self._emit("note", None, None, "HTTP 429 — backing off 60s")
                    time.sleep(60)
                    continue
                self._emit("note", None, None, f"http {e.code}")
                time.sleep(self.interval); continue
            except Exception as e:
                self._emit("note", None, None, f"poll error: {e}")
                time.sleep(self.interval); continue

            with self.lock:
                prev = self.current
                now = time.time()
                if prev:
                    for g in set(prev) - set(cur):
                        self.events.append((now, "vanish", g, prev[g]))
                        if not self.quiet:
                            print(f"{stamp(now)} · vanished  #{g}  last-known {maps(prev[g])}", flush=True)
                    for n in set(cur) - set(prev):
                        self.events.append((now, "appear", n, cur[n]))
                        if not self.quiet:
                            print(f"{stamp(now)} · reappeared #{n}  {maps(cur[n])}", flush=True)
                self.current = cur
            self.ready.set()
            time.sleep(self.interval)

    def _emit(self, kind, num, pos, msg):
        print(f"{stamp()} · {msg}", flush=True)

    def snapshot(self):
        with self.lock:
            return dict(self.current)

    def events_between(self, kind, t0, t1, number=None):
        with self.lock:
            return [e for e in self.events
                    if e[1] == kind and t0 <= e[0] <= t1 and (number is None or e[2] == number)]


def correlate(watcher, kind, mark, window_before, window_after, target=None):
    """Wait out the feed-refresh window, then return matching events near `mark`."""
    deadline = mark + window_after
    seen_key = set()
    while time.time() < deadline:
        hits = watcher.events_between(kind, mark - window_before, time.time(), number=target)
        for h in hits:
            k = (h[0], h[2])
            if k not in seen_key:
                seen_key.add(k)
        time.sleep(1)
    return sorted(seen_key, key=lambda k: abs(k[0] - mark)), \
        watcher.events_between(kind, mark - window_before, deadline, number=target)


def haversine(a, b):
    import math
    R = 6371008.8
    p1, p2 = math.radians(a[0]), math.radians(b[0])
    dp, dl = p2 - p1, math.radians(b[1] - a[1])
    h = math.sin(dp/2)**2 + math.cos(p1)*math.cos(p2)*math.sin(dl/2)**2
    return 2 * R * math.asin(math.sqrt(h))


def follow(target, lat, lon, op_id, client_name, follow_radius, interval):
    """Stream a scooter's position whenever it is visible, until Ctrl-C.

    Unlike 1/2 (which watch a small radius near home), follow does its OWN
    whole-city query for the target number, because a ride can end anywhere.
    While it is rented it is absent from the feed, so the stream shows gaps -
    'in a rental' - and prints a fresh position each time it reappears or moves.
    This is the persistent per-vehicle view: where one scooter goes over time,
    across successive riders, from public data alone.
    """
    print(f"  ▶ following #{target} across the whole city ({follow_radius/1000:.0f} km); Ctrl-C to stop")
    last_pos = None
    state = None
    try:
        while True:
            try:
                full = query(lat, lon, follow_radius, op_id, client_name)
            except Exception as e:
                print(f"{stamp()}  follow poll error: {e}", flush=True)
                time.sleep(max(3.0, interval)); continue
            p = full.get(target)
            now = time.time()
            if p:
                moved = None if last_pos is None else haversine(last_pos, p)
                if last_pos is None or (moved is not None and moved > 5):
                    extra = "" if moved is None else f"  (moved {moved:.0f} m)"
                    print(f"{stamp(now)}  #{target}  {loc(p)}  {p[2]/1000:.1f}km{extra}", flush=True)
                    last_pos = p
                elif state != "present":
                    print(f"{stamp(now)}  #{target}  parked {loc(p)}", flush=True)
                state = "present"
            else:
                if state != "absent":
                    print(f"{stamp(now)}  #{target}  — gone from feed (in a rental / picked up)", flush=True)
                    state = "absent"
                last_pos = None
            time.sleep(max(3.0, interval))
    except KeyboardInterrupt:
        print("\n  (stopped following; back to menu)\n")


def interactive(watcher, args):
    snap = watcher.snapshot()
    print(f"\n  warmed — {len(snap)} {args.operator} scooters cached within {args.radius} m.")
    print("\n  ┌──────────────────────────────────────────────────────┐")
    print("  │   1        = rental START  (then unlock in the app)  │")
    print("  │   2        = rental END    (then end in the app)     │")
    print("  │   3 [num]  = FOLLOW the identified scooter over time  │")
    print("  │   q        = quit                                    │")
    print("  └──────────────────────────────────────────────────────┘")
    print(f"  1/2 locate the scooter by the moment you act (feed refreshes ~30s,")
    print(f"  so I wait up to {args.window:.0f}s). 3 streams the ea-number's position across")
    print(f"  trips until you Ctrl-C. `3 370341` follows a specific number.\n")

    target = None
    start_pos = None
    end_pos = None

    while True:
        try:
            cmd = input("scoot> ").strip().lower()
        except (EOFError, KeyboardInterrupt):
            break
        if cmd in ("q", "quit", "exit"):
            break

        if cmd in ("1", "start"):
            mark = time.time()
            print(f"  [START marked {stamp(mark)}]  → UNLOCK IN THE APP NOW.  watching for the vanish (≤{args.window:.0f}s)…", flush=True)
            _k, evs = correlate(watcher, "vanish", mark, 6, args.window)
            if not evs:
                print("  …nothing vanished in the window. Try 1 again, or the feed is lagging.\n")
                continue
            # Spatial filter: your scooter is one you physically unlocked NEAR you,
            # so ignore citywide churn and keep only vanishes within pickup-radius
            # of --near. This is what cuts "45 vanished" down to the 1-2 near you.
            here = (lat, lon)
            near = [e for e in evs if haversine(here, e[3]) <= args.pickup_radius]
            dropped = len(evs) - len(near)
            if near:
                if dropped:
                    print(f"  ({dropped} distant vanishes ignored — too far to be the one you unlocked)")
                evs = near
            else:
                print(f"  (none within {args.pickup_radius:.0f} m of you; showing nearest of {len(evs)} citywide — widen --pickup-radius if wrong)")
                evs.sort(key=lambda e: haversine(here, e[3]))
                evs = evs[:3]
            evs.sort(key=lambda e: abs(e[0] - mark))
            best = evs[0]
            target, start_pos = best[2], best[3]
            print(f"  ★ PICKED UP scooter #{target}")
            print(f"    START position: {loc(start_pos)}  ({start_pos[2]/1000:.1f} km battery)")
            if len(evs) > 1:
                print(f"    (note: {len(evs)} vanished in the window — picked the closest in time; others:")
                for e in evs[1:]:
                    print(f"        #{e[2]} {stamp(e[0])} ({e[0]-mark:+.0f}s) {maps(e[3])}")
                print(f"     if #{target} isn't yours, press 2 at end and compare where each reappears.)")
            print()

        elif cmd in ("2", "end"):
            mark = time.time()
            who = f"#{target}" if target else "your scooter"
            print(f"  [END marked {stamp(mark)}]  → END THE RIDE IN THE APP NOW.  watching for {who} to reappear (≤{args.window:.0f}s)…", flush=True)
            _k, evs = correlate(watcher, "appear", mark, 6, args.window, target=target)
            if not evs and target:
                # fall back to any reappearance if the target-specific watch found nothing
                _k, evs = correlate(watcher, "appear", mark, 6, 10)
            if not evs:
                print("  …no reappearance yet (end-ride settling can take a few minutes). Press 2 again shortly.\n")
                continue
            evs.sort(key=lambda e: abs(e[0] - mark))
            chosen = next((e for e in evs if target and e[2] == target), evs[0])
            end_pos = chosen[3]
            print(f"  ★ DROPPED OFF scooter #{chosen[2]}")
            print(f"    END position: {loc(end_pos)}  ({end_pos[2]/1000:.1f} km battery)")
            if len(evs) > 1:
                print(f"    (other reappearances in the window:")
                for e in evs:
                    if e is not chosen:
                        print(f"        #{e[2]} {maps(e[3])}")
                print("     )")
            if start_pos and end_pos:
                d = haversine(start_pos, end_pos)
                print(f"\n  ══ TRIP #{target or chosen[2]} ══")
                print(f"    start: {maps(start_pos)}")
                print(f"    end  : {maps(end_pos)}")
                print(f"    straight-line distance: {d:.0f} m")
            print()

        elif cmd == "3" or cmd == "follow" or cmd.startswith("3 ") or cmd.startswith("follow "):
            parts = cmd.split()
            num = parts[1] if len(parts) > 1 else target
            if not num:
                print("  no scooter identified yet — press 1 first, or `3 <number>`.\n")
                continue
            target = num
            follow(num, lat, lon, OPERATORS[args.operator], args.client_name, args.follow_radius, args.interval)
        elif cmd in ("status", "?"):
            snap = watcher.snapshot()
            print(f"  {len(snap)} visible; target #{target or '—'}; "
                  f"start {'set' if start_pos else '—'}; end {'set' if end_pos else '—'}\n")
        elif cmd == "":
            continue
        else:
            print("  1 = start · 2 = end · 3 = follow · q = quit\n")

    watcher.stop = True
    print("bye")


def passive(watcher, args):
    watcher.ready.wait(timeout=30)
    snap = watcher.snapshot()
    scope = ",".join(sorted(watcher.track)) if watcher.track else f"{len(snap)} within {args.radius}m"
    print(f"{stamp()} watching {args.operator}: {scope} (interval {args.interval}s)", flush=True)
    for n in sorted(snap):
        print(f"{stamp()}   present #{n}  {maps(snap[n])}  {snap[n][2]/1000:.1f}km", flush=True)
    t0 = time.time()
    while time.time() - t0 < args.duration:
        time.sleep(1)
    watcher.stop = True


def main():
    ap = argparse.ArgumentParser(description="Watch the Entur feed and identify your scooter.")
    ap.add_argument("--near", metavar="LAT,LON", default="59.9139,10.7522")
    ap.add_argument("--radius", type=int, default=500, metavar="M",
                    help="watch radius for 1/2 pickup detection near --near (default 500m)")
    ap.add_argument("--follow-radius", type=int, default=30000, metavar="M",
                    help="radius option 3 uses to follow a bike across the city (default 30000m = 30km)")
    ap.add_argument("--track", metavar="N,N", default="")
    ap.add_argument("--operator", default="ryde", choices=list(OPERATORS))
    ap.add_argument("--interval", type=float, default=3.0, metavar="S")
    ap.add_argument("--window", type=float, default=40.0, metavar="S",
                    help="how long to wait after a start/end mark for the feed to reflect it (default 40s; the feed refreshes ~30s so this can't go much lower)")
    ap.add_argument("--pickup-radius", type=float, default=200.0, metavar="M",
                    help="for 1/2: only consider vanishes/appears within this distance of --near, "
                         "since you unlock a scooter next to you (default 200m). This is what cuts "
                         "citywide rush-hour churn down to your scooter.")
    ap.add_argument("--client-name", default="scoot-watch")
    ap.add_argument("--interactive", action="store_true", help="prompt-driven identify-my-scooter mode")
    ap.add_argument("--for", dest="duration", type=float, default=10800, metavar="S")
    args = ap.parse_args()

    lat, lon = (float(x) for x in args.near.split(","))
    track = set(x.strip() for x in args.track.split(",") if x.strip())
    watcher = Watcher(lat, lon, args.radius, OPERATORS[args.operator],
                      args.client_name, args.interval, track)
    if args.interactive:
        watcher.quiet = True
    watcher.start()
    watcher.ready.wait(timeout=30)
    try:
        if args.interactive:
            interactive(watcher, args)
        else:
            passive(watcher, args)
    except KeyboardInterrupt:
        watcher.stop = True


if __name__ == "__main__":
    main()
