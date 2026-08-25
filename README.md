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
- **The result set caps at 500.** Exactly 500 back means the list was truncated;
  `scootless` reports `500+` rather than pretending the number is exact.
- **`isReserved` appears to be permanently `false`.** An active rental seems to
  remove the vehicle from the feed entirely instead.
- **The GraphQL endpoint takes raw coordinates**, so it spans city systems
  automatically — you never pick a feed. The per-city GBFS feeds are only needed
  for whole-city snapshots.

### How fresh is it?

Measured rather than assumed — polling one city every 20 s for three minutes:

```
t=   0.3s last_updated=1787646208 n=4741
t=  20.7s last_updated=1787646208 n=4741 | gone= 0 new= 0 moved= 0
t=  41.0s last_updated=1787646236 n=4738 | gone=12 new= 9 moved=12
t=  61.4s last_updated=1787646266 n=4736 | gone=12 new=10 moved=10
t=  81.7s last_updated=1787646266 n=4736 | gone= 0 new= 0 moved= 0
t= 102.0s last_updated=1787646297 n=4742 | gone= 4 new=10 moved= 6
t= 122.7s last_updated=1787646328 n=4736 | gone=13 new= 7 moved=38
t= 143.0s last_updated=1787646358 n=4742 | gone=11 new=17 moved=30
t= 163.3s last_updated=1787646358 n=4742 | gone= 0 new= 0 moved= 0
t= 183.7s last_updated=1787646388 n=4741 | gone=10 new= 9 moved=78
```

`last_updated` steps in exact 30-second increments (…208, 236, 266, 297, 328,
358, 388), and every tick carries real churn: roughly 10 vehicles appearing and
10 disappearing city-wide, with dozens more having moved. Poll on a 20 s cycle
and you simply see the same snapshot twice.

This is the same data the operators' own maps draw from, so **polling faster
than ~30 s gains you nothing.**

## Courtesy

This reads a public, openly licensed dataset published for exactly this kind of
use. Identify yourself with `ET-Client-Name`, and don't poll faster than the
~30 s at which the data actually changes.

Not affiliated with Ryde, Voi, Bolt, Dott or Entur.
