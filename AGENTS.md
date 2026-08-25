# scootless — notes for agents

Read [`README.md`](README.md) for what this is. This file is what you need to
work on it without rediscovering the sharp edges.

## Layout

```
scootless.py          the one-shot CLI: Python 3.8+, stdlib only, no build step
cmd/scootlessd/       the daemon
internal/entur/       upstream API client, operator registry, vehicle model
internal/geo/         distance, bearing, compass, bounding circles
internal/store/       SQLite: fences, watches, samples, presence, nearest
internal/poll/        the tick - the only thing that talks upstream
internal/notify/      sinks: ntfy, MQTT, log, and a fan-out
internal/api/         HTTP surface
internal/config/      .env and SCOOTLESS_* loading
docs/                 IDEA.md (product), BACKEND.md (design), APP.md (the app)
deploy/               systemd user unit
```

## Commands

```
make build          build bin/scootlessd
make test           the suite - offline and deterministic
make test-live      also exercises the real API (SCOOTLESS_LIVE=1)
make install        build, install the user unit, restart
make restart        build, then restart
make logs           follow the journal
```

## Things that will bite you

**Rebuild before restarting.** The unit runs a prebuilt binary. `systemctl
restart` on its own reruns the stale build and looks exactly like a config
change that did not take effect. Use `make restart` / `make install`.

**Do not put coordinates in code, tests, or logs.** They live in `.env`, which
is gitignored. The daemon logs them only under `-v`, and the notification
payload deliberately carries a fence's name and radius but not its position -
there is a test asserting that.

**`.env` also holds credentials** (ntfy topic, MQTT password). On a public ntfy
server the topic name *is* the credential. Never echo these into a terminal, a
commit, or a transcript.

**This repository is public.** Before pushing, check tracked files *and* commit
messages for hostnames, private IP ranges, domains and personal addresses.

## Upstream API facts, measured

These cost real time to establish. Do not re-derive them, and do not assume the
opposite:

- **Operator IDs are case-sensitive**, and an unknown one returns an *empty
  list, not an error* - so a typo is indistinguishable from "no scooters here".
  `entur.OperatorIDs` rejects unknown keys before any request is sent.
- **`count` selects the nearest N**, it does not truncate arbitrarily. Nested
  queries return strictly nested sets. Rows come back *unordered*, so sort them
  yourself. Hitting the 500 cap never costs you the closest vehicle.
- **Voi returns bicycles** in the same feed as its scooters. Filter on
  `formFactor`, or a city bike is reported as a scooter.
- **`currentFuelPercent` is a 0-1 fraction**, and Ryde leaves it null on every
  vehicle. `nil` must stay distinct from zero: reporting nothing is not a flat
  battery.
- **`isDisabled` vehicles are listed but cannot be rented.**
- **`ttl` is not the update interval.** Measured: Ryde advances every 27-33 s
  while advertising 5; Voi is bimodal at ~30 s or ~59 s; Bolt moves only every
  ~5 minutes. Each operator runs on its own phase, so there is no clock to
  synchronise to, and the GraphQL endpoint exposes no timestamp of its own.
- **Operator coverage is per city.** Dott runs in Trondheim, not Oslo. An
  absent operator is normal, not a fault.

## Invariants worth preserving

- **A tick must not fail because a nicety failed.** A nearest-vehicle probe or
  a notification delivery that errors is logged and skipped; the watches depend
  on the tick completing.
- **`MarkFired` disarms in the same statement that records the fire**, so two
  overlapping ticks cannot both notify.
- **An appearance watch fires on its baseline diff, never on a count.**
- **Distances are re-based onto the fence** when a coalesced query's results are
  trimmed to it. The client fills in distance relative to whatever point was
  queried, which for a coalesced query is nobody's doorstep.
- **Presence tolerates a short disappearance.** A vehicle blinking out of the
  feed and back is common; reporting each blink as an arrival would make the
  whole feature untrustworthy.
- **Zero counts are recorded explicitly.** A missing row is indistinguishable
  from a missed poll.

## Testing

The default suite is offline: the upstream client is exercised against an
`httptest` server, and the store against `:memory:`. Live tests are behind
`SCOOTLESS_LIVE=1` so the suite never depends on what is parked in Oslo today.

Do not weaken an assertion to make a test pass without establishing that the
assertion was wrong. Two real bugs here were found by tests failing for reasons
that looked like test artifacts and were not.
