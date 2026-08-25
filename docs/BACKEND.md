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

**Phase-lock to the feed's own clock.** The upstream feed advances
`last_updated` in exact 30-second steps. Polling on a free-running 30 s timer
means a scooter can appear immediately after a poll and go unreported for
almost a full cycle. Instead, read `last_updated`, and schedule the next poll
for roughly a second after the next expected step. Same request rate, but
worst-case latency drops from ~30 s to a second or two — which is the entire
difference between "I need a scooter" being useful and being a novelty.

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
