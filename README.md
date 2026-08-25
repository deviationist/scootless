# scootless

Are there any scooters left within *x* metres of you — and if not, tell me the
moment one turns up.

It covers **Ryde, Voi, Bolt and Dott** through Norway's open mobility data, and
answers the two questions that actually come up:

**Before you put your shoes on** — *is there anything out there?* One shot, no
state, answered in under a second.

```
$ scootless
0 rentable scooters within 100 m of home
  You are scootless. Widen the radius with -r, or try -o all.
```

**When there is nothing out there** — you have already checked all three apps,
the block is empty, and standing there refreshing them is not a plan. Arm a
watch and put the phone away:

```
$ curl -XPOST localhost:8099/api/v1/watches \
    -d '{"kind":"appearance","fence_id":"home","operators":["ryde"]}'
```

The next scooter to appear inside your radius sends a notification you can tap
straight into the operator's app, on that exact vehicle. That second question is
the one the operators' own apps cannot answer, and it is why this exists: by
mid-morning a whole block can be stripped of one operator while the others are
still standing there, and if your subscription only pays for one brand, that
brand's count is the only one that matters.

## The two pieces

| | |
|---|---|
| **`scootless.py`** | The one-shot CLI. Python 3.8+, standard library only, no build step. Answers the first question and nothing else. |
| **`scootlessd`** | The daemon. Polls, keeps history, holds watches, and notifies. Go, single static binary, SQLite. |
| **`scootless-track`** | Follows vehicles you name and reports where they go. |

The CLI needs nothing running, and the daemon needs no CLI.

`scootless-track` exists because of one property of the feed: **an active rental
removes a vehicle rather than flagging it.** So a vehicle that can no longer be
looked up is in use, and one that reappears elsewhere has been parked there —
which is what separates "somebody rode it away" from "a van collected it".

```
$ scootless-track YRY:Vehicle:ea3...
21:17:07  parked      YRY:Vehicle:ea359795…  range_km=32
21:31:27  vanished    YRY:Vehicle:ea359795…  note=in use, or withdrawn
21:44:07  reappeared  YRY:Vehicle:ea359795…  moved_m=1840 bearing=NE gone_for_s=760
```

It follows only the identifiers it is given. It is not a way to log a city, and
should not become one.

## Quick start

The CLI:

```bash
git clone https://github.com/deviationist/scootless.git
cd scootless
cp .env.example .env      # then edit .env
./scootless.py
```

The daemon:

```bash
make build
./bin/scootlessd --once   # one poll, then exit
./bin/scootlessd          # poll continuously, serve the API
```

`.env` is gitignored. Your home coordinates never enter the repository, and the
daemon logs them only under `-v`.

## Configuration

Settings come from, in increasing order of precedence: built-in defaults,
`~/.config/scootless/config.json` (written by `--save-location`, CLI only),
`.env` or `SCOOTLESS_*` environment variables, and command-line flags.

Every setting is documented in [`.env.example`](.env.example). The ones you
need to start:

| Variable | Meaning |
|---|---|
| `SCOOTLESS_LAT` / `SCOOTLESS_LON` | Where you're standing, decimal degrees |
| `SCOOTLESS_RADIUS` | Search radius in metres |
| `SCOOTLESS_OPERATOR` | `ryde`, `voi`, `bolt`, `dott`, `all`, or a subset |
| `SCOOTLESS_NTFY_SERVER` / `_TOPIC` | Where notifications go |

## The CLI

```bash
scootless                        # your configured defaults
scootless -r 250 -t 5            # wider radius, higher alarm
scootless -o ryde                # only the brand you subscribe to
scootless --min-battery-km 10    # ignore nearly-flat scooters
scootless --json                 # machine-readable
scootless --quiet                # just the number
```

### Exit codes

Built for cron and shell pipelines:

| Code | Meaning |
|-----:|---------|
| `0`  | Plenty available |
| `10` | Scarce — at or below your threshold |
| `11` | None at all |
| `2`  | Could not reach the API |

```bash
scootless --quiet >/dev/null || echo "running out!" | your-notifier
```

## The daemon

### Watches

A **watch** is a standing request to be told when something changes.

- **appearance** — fire when a vehicle turns up that was not there when the
  watch was armed. This is the *"I need a scooter"* case. It is an edge trigger
  over a baseline captured at arm time, so arming it while one is already
  parked there does not fire instantly.
- **scarcity** — fire when the count drops to a threshold or below. Planning,
  rather than rescue.

Both fire once and disarm by default, and both carry a TTL. Nothing polls
forever.

### HTTP API

Binds to `127.0.0.1:8099`. Set `SCOOTLESS_API_TOKEN` to require a bearer token
before exposing it anywhere else.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/vehicles` | the one-shot ask — `lat`, `lon`, `radius`, `operators` |
| `GET` | `/api/v1/status` | counts *and* nearest-per-operator for a fence, with a human summary |
| `GET`/`POST` | `/api/v1/fences` | places being watched |
| `GET`/`POST` | `/api/v1/watches` | list, or arm one |
| `DELETE` | `/api/v1/watches/{id}` | cancel |
| `GET` | `/api/v1/history` | counts over time |
| `GET` | `/api/v1/arrivals` | when vehicles turned up |
| `GET` | `/healthz` | liveness |

`/api/v1/status` is the one worth seeing:

```json
{ "summary": "2 Bolt available, 173 m to nearest Voi, 481 m to nearest Ryde" }
```

A bare zero is a bad answer. "None here" and "none within five kilometres" call
for different decisions, and so does "none here, but one 173 m away" — which is
a walk, not a defeat.

### History

Every poll is recorded, which answers better questions than any alarm does:

- **`sample`** — the count per operator per tick. *When does the block empty,
  and when does it refill?*
- **`presence`** — one row per continuous stint a vehicle spends inside the
  fence. *How often does one actually turn up, and how long does it last?*
- **`nearest`** — how far the closest one was, including when it is outside the
  fence entirely. *How wide is the drought?*

### Notifications

Fired watches fan out to every configured sink, and a failure in one does not
stop the others:

- **ntfy** — the hop that reaches a phone. The notification's tap action is the
  operator's own deep link, so it is one tap from a ride.
- **MQTT** — a bus rather than a destination, so anything else can subscribe.
- **the log** — always, so a fired watch is visible even when delivery is not.

With none configured it logs only, which is enough to develop against.

### Running it

```bash
make install     # build, install the user unit, restart
make logs
```

[`deploy/scootlessd.service`](deploy/scootlessd.service) is a systemd user unit.
Use `make install` rather than restarting by hand — the unit runs a prebuilt
binary, so a bare restart will happily rerun a stale build.

## Where the data comes from

No reverse engineering, no proxying a phone app, no API key. Norwegian
micromobility operators are required to publish their live fleet as a
[GBFS](https://gbfs.org) feed, and **Entur**, the national transport data hub,
aggregates every operator into one API.

The useful endpoint is Entur's GraphQL service, because it does the radius
filtering server-side:

```graphql
{
  vehicles(lat: 59.9139, lon: 10.7522, range: 100,
           operators: ["YRY:Operator:Ryde"]) {
    id lat lon currentRangeMeters isDisabled isReserved
  }
}
```

`POST https://api.entur.io/mobility/v2/graphql`, with a courtesy
`ET-Client-Name` header. That is the entire authentication story.

### Things worth knowing before building on this

Findings from probing the live API, each of which cost time to discover:

- **Operator IDs are case-sensitive.** `YRY:Operator:Ryde` works;
  `YRY:Operator:ryde` silently returns an empty list rather than an error.
- **`isDisabled` vehicles appear in the feed but cannot be rented.** Only ~13 of
  4,741 in Oslo, but filter them or you will walk to a dead scooter.
- **`currentFuelPercent` is a 0–1 fraction, not a percentage.** Multiply by 100.
- **Ryde never populates `currentFuelPercent`** — it is `null` on every one of
  its vehicles, while Voi and Bolt always fill it. For Ryde,
  `currentRangeMeters` is the only battery signal available.
- **The result set caps at 500 — but `count` selects the *nearest* N.** It is
  not an arbitrary truncation. Asking for 10, 25, 100 and 300 vehicles at a
  fixed radius returns strictly nested sets — no vehicle in a smaller result
  was ever missing from a larger one — with the furthest distance growing
  101 m, 121 m, 194 m, 336 m. The rows come back *unordered*, so you must sort
  them yourself, but nothing nearer than the furthest row was withheld. Two
  consequences: hitting the cap never costs you the closest scooter, and a tiny
  `count` at a wide radius is an exact nearest-vehicle lookup rather than a
  sample. `scootless` still reports `500+` rather than pretending the total is
  exact.
- **`isReserved` appears to be permanently `false`.** An active rental seems to
  remove the vehicle from the feed entirely instead.
- **The GraphQL endpoint takes raw coordinates**, so it spans city systems
  automatically — you never pick a feed. The per-city GBFS feeds are only needed
  for whole-city snapshots.
- **Vehicles can be looked up by id**, singly or in batches, with no radius and
  so no nearest-N selection — a vehicle kilometres away comes back as readily as
  one outside the door. An unknown id yields a clean `null` rather than an
  error, which makes "gone from the feed" unambiguous.
- **Operator coverage is per city, and an absent operator is not an error.**
  Dott runs in Trondheim but has nothing in Oslo; Bolt is Oslo-only of the
  cities checked. A valid operator with no vehicles returns an empty list that
  looks exactly like a wrong ID — which is why `scootless` validates operator
  keys locally before asking.

### How fresh is it?

Measured rather than assumed — and the first measurement was wrong, which is
worth keeping in the open. Sampling **one** operator every 20 s suggested
`last_updated` advanced in exact 30-second steps. Sampling **all three** Oslo
operators every 5 s shows it does not: a 20 s cycle aliases a jittery ~30 s
tick into a suspiciously clean one.

| Operator | advertised `ttl` | measured step |
|---|---|---|
| Ryde | 5 s | 27–33 s, mean 30.0 — jitter, not a clock |
| Voi | 30 s | either ~30 s or ~59 s |
| Bolt | 300 s | ~312 s |

Pooled step sizes from two independent runs:

```
Ryde  27s #   28s ####   29s ##########   30s ############   31s ##########   32s ####   33s #
Voi   30s ####  31s ##  32s ##       55s #  58s ###  59s ####  60s ####  61s ###  62s ##
Bolt  ~312s
```

Ryde is unimodal around 30 s. Voi is **bimodal**, not noisy: it runs on the same
30 s grid but publishes only about every third tick, for an effective ~50 s.
Bolt moves roughly every five minutes, despite advertising a `ttl` of 300 that
would suggest the same thing far more precisely than it delivers.

Three things follow. **Each operator runs on its own phase**, so there is no
single clock to synchronise a poller to — and the GraphQL endpoint exposes no
timestamp of its own to read one from, only the per-city GBFS feeds do.
**`ttl` is not the update interval**; Ryde advertises 5 s and moves every 30.
And **an appearance alert on Bolt can never be responsive**, because the data
underneath it is five minutes old at worst. That is a property of the feed, not
of any tool built on it.

Polling every ~20 s catches every Ryde tick without aliasing. Faster than that
gains nothing at all.

## Courtesy

This reads a public, openly licensed dataset published for exactly this kind of
use. Identify yourself with `ET-Client-Name`, and don't poll faster than the
~20 s below which there is simply nothing new to read.

Not affiliated with Ryde, Voi, Bolt, Dott or Entur.

## License

MIT — see [LICENSE](LICENSE).
