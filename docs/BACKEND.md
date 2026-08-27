# scootless backend — design

> Status: design. No code yet. Implementation language is Go; nothing is built
> until a toolchain exists on the target host.

Companion to [IDEA.md](IDEA.md), which states the problem. This file states the
shape precisely enough that writing it is mechanical.

## Responsibilities

1. **Ask** — answer "what is rentable within *R* metres of here, right now".
2. **Watch** — hold armed watches, evaluate them on every tick, notify on a hit.
3. **Remember** — persist enough history to answer *when* the block empties and
   *when* it refills.

Everything else (the last hop of a notification, the UI) sits outside.

## The poll loop

One loop drives everything. It is the only thing that talks upstream.

**A fixed interval, because there is no clock to lock onto.** This design
originally called for phase-locking the poller to the feed's own 30-second
step, on the strength of the README's claim that `last_updated` advances in
exact 30-second increments. Measuring all three Oslo operators rather than one
showed that claim does not hold, and the idea does not survive it:

| Operator | advertised `ttl` | measured step |
|---|---|---|
| Ryde | 5 s | 26–33 s, mean 30 |
| Voi | 30 s | irregular, 26–58 s |
| Bolt | 300 s | slow and uneven — see below |

Three findings follow. Each operator ticks on **its own phase**, so there is no
single clock to lock onto. Ryde **jitters** either side of 30 s rather than
stepping exactly, so even a per-operator lock would drift. And **Bolt is the
slowest**, so an appearance watch on it lags one on Ryde.

Bolt's "~5 minutes" came from a coarse sample and did not survive re-measurement
at 2 s resolution: a 54 s gap, then three changes inside 4 s. It is still the
laggard, but 300 s is not a floor to design against.

**The interval is set by the aggregate change rate, not by any one operator.** A
watch is usually waiting for whichever scooter turns up first, so what matters is
how often *something* moves. Sampling all operators at 2 s for 200 s, the gaps
between consecutive changes were 4, 26, 28, 4, 32, 22, 2, 2, 28 and 6 seconds —
a mean of about **15 s**.

A fixed interval adds a uniform 0-to-interval delay on top of the feed's own
staleness, half of it on average. The original 20 s was therefore *undersampling*
a feed that was already moving, and cost roughly 10 s per notification for
nothing. **10 s** is a little faster than the feed changes and halves that term;
the floor is 5 s, below which we are asking a free public dataset several times
per change it makes to buy 2.5 s.

The GraphQL endpoint carries no timestamp of its own, so phase-locking would
mean a second request per operator per tick to read a GBFS header — more load
than simply sampling a bit faster, for a lock that Ryde's jitter would drift out
of anyway.

## What a tick costs, and what it must not cost

One tick is **one** upstream request per *group* of fences, not per fence, and
overlapping fences coalesce into a single query (see below). Measured, that
request is ~230 ms, of which ~35 ms is the TLS handshake when the connection is
not reused — so the client keeps a pooled transport sized for a whole tick's
fan-out.

Three rules keep a tick short, and they are about *blocking*, not about doing
less work:

**Queries fan out; results are consumed in order.** The group queries run
concurrently, and so do the per-operator nearest probes. Results are then read
back by index, so what gets stored does not depend on which request returned
first. The bug this invites is a crossed wire — one group's vehicles recorded
against another group's fences — which is why there is a test that gives each
fence a vehicle only *it* can legally contain.

**One group's failure is not the tick's failure.** Groups are independent sets of
fences. A failed query is collected and reported once every healthy group has
been recorded and its watches evaluated. Returning early instead means one
unlucky fence silences every other fence's watches for that tick.

**Delivery happens off the tick.** By the time a notification is dispatched the
watch has already fired, been recorded and been disarmed, so nothing left to
decide depends on the delivery — but it is a network round trip, and inline it
made a second watch firing on the same tick queue behind the first one's broker
acknowledgement. It is dispatched to its own goroutine with `context.WithoutCancel`
and its own timeout, because a tick's context dies with the tick and a
notification cancelled at that moment is one the phone never gets. Anything that
exits after ticking must call `Poller.Wait`.

The sinks fan out too: a broker and an ntfy server are independent destinations,
so the total is the slowest of them rather than the sum.

**Coalesce queries.** One upstream request per armed watch per tick does not
scale and is rude to a free dataset. Group active fences: overlapping or nearby
fences become a single query with a radius covering all of them, filtered
client-side by exact distance. The geometry is free; the HTTP call is not.

**Filter on `formFactor`.** Voi returns bicycles in the same vehicle feed as its
scooters. Anything not `SCOOTER_STANDING` must be excluded, or a city bike will
be reported as a scooter. Also exclude `isDisabled` — listed, but not rentable.

## Watch lifecycle

```
                arm
                 |
                 v
   +---------> armed ------ match ------> fired
   |             |                          |
   |             |-- now > expires_at --> expired
   |             |-- DELETE ------------> cancelled
   |                                        |
   +----------- repeat = true --------------+
```

- **Baseline.** At arm time, record the set of vehicle IDs already inside the
  fence. An appearance watch fires on `current \ baseline`, never on a count.
  Usually the baseline is empty — that is why the watch was armed — but
  recording it is what stops a mis-timed arm from firing instantly.
- **Fire once by default.** `repeat = false` disarms on the first hit. Repeating
  watches exist but are opt-in, and still respect a cooldown.
- **Always expires.** No watch polls forever. Default TTL ~30 minutes.

A **scarcity** watch is the same object with a different predicate: fire when
the count inside the fence falls to `threshold` or below. It shares the loop,
the store and the notifier.

## Storage

SQLite, one file, pure-Go driver so there is no cgo and no toolchain on the
target.

```sql
CREATE TABLE fence (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  lat       REAL NOT NULL,
  lon       REAL NOT NULL,
  radius_m  INTEGER NOT NULL
);

CREATE TABLE watch (
  id           TEXT PRIMARY KEY,
  device       TEXT NOT NULL,
  kind         TEXT NOT NULL,          -- appearance | scarcity
  fence_id     TEXT NOT NULL REFERENCES fence(id),
  operators    TEXT NOT NULL,          -- json array
  min_range_m  INTEGER NOT NULL DEFAULT 0,
  threshold    INTEGER,                -- scarcity only
  baseline     TEXT NOT NULL,          -- json array of vehicle ids at arm time
  repeat       INTEGER NOT NULL DEFAULT 0,
  state        TEXT NOT NULL,          -- armed | fired | expired | cancelled
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  fired_at     INTEGER
);

CREATE TABLE event (
  id        INTEGER PRIMARY KEY,
  watch_id  TEXT NOT NULL REFERENCES watch(id),
  at        INTEGER NOT NULL,
  payload   TEXT NOT NULL              -- json: the matched vehicles
);

-- One row per fence per operator per tick. The cheap history.
CREATE TABLE sample (
  fence_id  TEXT NOT NULL REFERENCES fence(id),
  at        INTEGER NOT NULL,
  operator  TEXT NOT NULL,
  count     INTEGER NOT NULL,
  PRIMARY KEY (fence_id, at, operator)
);

-- Arrivals and departures, derived on the fly. Far smaller than storing every
-- vehicle on every tick, and it is what answers "how often does one show up".
CREATE TABLE presence (
  fence_id   TEXT NOT NULL REFERENCES fence(id),
  vehicle_id TEXT NOT NULL,
  operator   TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  PRIMARY KEY (fence_id, vehicle_id, first_seen)
);
```

`presence` is the important one. A row whose `first_seen` is later than the
fence's own start is an **arrival** — exactly the event the watch feature sells,
and the thing we currently have no measurement of.

## HTTP API

Versioned from the start, JSON throughout.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/vehicles` | the ask — `lat`, `lon`, `radius`, `operators`, `min_range_m` |
| `GET` | `/api/v1/fences` | list saved fences |
| `POST` | `/api/v1/fences` | create one |
| `GET` | `/api/v1/watches` | list, filterable by `device` and `state` |
| `POST` | `/api/v1/watches` | **arm** — "I need a scooter" |
| `DELETE` | `/api/v1/watches/{id}` | cancel |
| `GET` | `/api/v1/watches/{id}/events` | what it fired, and when |
| `GET` | `/api/v1/history` | counts over time — `fence`, `from`, `to`, `bucket` |
| `GET` | `/api/v1/arrivals` | arrival events for a fence over a window |
| `GET` | `/healthz` | liveness, last successful upstream poll |

`POST /api/v1/watches` is the whole product in one call: a fence (saved, or an
ad-hoc lat/lon/radius from the phone's own position), an operator set, a TTL,
and whether it repeats.

## Notification

The daemon **publishes to MQTT and stops there**. A bus, not a destination — so
Web Push, ntfy, Telegram and e-mail become subscribers rather than rewrites.

```
scootless/watch/<watch-id>/fired    the payload below
scootless/fence/<fence-id>/sample   live counts, for anything that wants them
```

```json
{
  "watch_id": "…",
  "kind": "appearance",
  "fired_at": 1787647228,
  "fence": {"name": "home", "radius_m": 150},
  "vehicles": [
    {"id": "…", "operator": "Ryde", "distance_m": 61, "bearing": "W <",
     "range_km": 36.2, "battery_pct": null, "app_link": "…"}
  ]
}
```

Sinks live behind one interface so a second one is a new file, not a change.

## Configuration

Environment and `.env`, as today. Coordinates, broker credentials and any
deployment detail stay in gitignored files and never enter the repository.

## Open, and deliberately not decided here

- **Auth for the phone.** An interactive login every morning defeats the
  feature; a long-lived per-device token probably does not.
- **Single or multi user.** The schema keys watches by `device` from the start,
  so multi-user is a feature rather than a migration — but nothing else assumes
  it.
- **Whether the Python CLI keeps its own upstream client** or starts calling
  this API when one is reachable.
