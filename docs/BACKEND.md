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
| Ryde | 5 s | 29–33 s, mean 30 |
| Voi | 30 s | irregular, 30–62 s |
| Bolt | 300 s | ~317 s |

Three findings follow. Each operator ticks on **its own phase**, so there is no
single clock to lock onto. Ryde **jitters** either side of 30 s rather than
stepping exactly, so even a per-operator lock would drift. And **Bolt updates
roughly every five minutes**, which puts a floor on notification latency for
that operator that no polling strategy can lower.

The honest replacement is a fixed interval a little under the fastest
operator's cadence — 20 s — which catches Ryde's ticks without aliasing and
accepts that for the slower operators the upstream cadence dominates. The
GraphQL endpoint carries no timestamp of its own, so the alternative would mean
a second request per operator per tick to read a GBFS header, for latency that
is bounded upstream anyway.

Worth stating in the product: an appearance watch on Bolt cannot be as
responsive as one on Ryde, and that is a property of the data, not the tool.

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
