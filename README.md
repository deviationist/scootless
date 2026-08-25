# scootless

Are there any scooters left within *x* metres of you — and should you hurry?

A one-shot CLI that answers the question you have before you put your shoes on.
It covers **Ryde, Voi, Bolt and Dott**, sorts by walking distance, and warns you
when the count drops low enough that you should claim one now rather than after
your coffee.

```
$ scootless -r 100 -t 3
3 rentable scooters within 100 m of home
  SCARCE - at or below your threshold of 3. Grab one before it's gone.
    61 m W <   36.2 km  Ryde
    80 m SE\   12.6 km 27%  Bolt
    97 m N ^   25.4 km  Voi

$ scootless
0 rentable scooters within 100 m of home
  You are scootless. Widen the radius with -r, or try -o all.
```

It exists because by mid-morning an entire city block can be stripped of one
operator's scooters while the others are still standing there. If your
subscription only pays for one brand, the count that matters is that brand's.

## Install

Python 3.8+, standard library only. No dependencies, no build step.

```bash
git clone https://github.com/deviationist/scootless.git
cd scootless
cp .env.example .env      # then edit .env
./scootless.py
```

Optionally put it on your `PATH`:

```bash
ln -s "$PWD/scootless.py" ~/.local/bin/scootless
```

## Configuration

Settings come from three places. Later ones win:

1. built-in defaults
2. `~/.config/scootless/config.json` — written by `--save-location`
3. `.env` in the project directory, or `SCOOTLESS_*` environment variables
4. command-line flags

`.env` is gitignored. Your home coordinates never enter the repository.

| Variable | Meaning |
|---|---|
| `SCOOTLESS_LAT` / `SCOOTLESS_LON` | Where you're standing, decimal degrees |
| `SCOOTLESS_RADIUS` | Search radius in metres |
| `SCOOTLESS_THRESHOLD` | Warn at or below this many |
| `SCOOTLESS_OPERATOR` | `ryde`, `voi`, `bolt`, `dott`, `all`, or a subset |
| `SCOOTLESS_CLIENT_NAME` | Sent as `ET-Client-Name` |

If you'd rather not use `.env`, save a location instead:

```bash
scootless --save-location home 59.9139,10.7522
```

## Usage

```bash
scootless                        # your configured defaults
scootless -r 250 -t 5            # wider radius, higher alarm
scootless -o ryde                # only the brand you subscribe to
scootless -o voi,bolt            # only the ones you name
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

Running it periodically on an always-on machine turns it from a question you
ask into a warning you receive. A `systemd` timer, a cron entry, or any
scheduler that can read an exit code will do; the tool holds no state, so
running it every few minutes costs nothing.

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
