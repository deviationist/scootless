# scootless — the idea

> Status: design sketch. Nothing here is built yet except the one-shot CLI
> (`scootless.py`), which covers exactly one of the two moments below.

## The problem, stated properly

There are **two different moments** where you need to know about scooters, and
they want opposite things from a tool.

**Moment 1 — before you put your shoes on.** *Is there anything out there?*
A question you ask. One-shot, pull, answered in under a second. This is what
`scootless.py` already does.

**Moment 2 — there is nothing out there.** You have already opened all three
apps. The block is empty. You are now stuck: you can stand around refreshing
apps, or you can give up and walk. What you actually want is to say **"I need a
scooter"** once, put the phone in your pocket, and be told the moment one
appears inside your radius — someone ending a ride, or a rebalancing van
dropping a batch.

Moment 2 is the real product. It is also the one that cannot be solved by
opening the operators' own apps, which is what makes it worth building.

## What it does

### Ask (pull)
"How many rentable scooters are within *R* metres of me, from the operators I
actually pay for?" Sorted by walking distance. Available as CLI, HTTP API and
in the app.

### Watch (push)
Arm a watch and get notified when the situation changes in your favour.

- **Appearance watch** — *"I need a scooter."* Notify me the moment a rentable
  vehicle appears inside my geofence. This is an **edge trigger**: it fires on a
  vehicle that was not there when the watch was armed, not on a level.
- **Scarcity watch** — the inverse, for planning rather than rescue. Notify me
  when the count inside my fence drops to *T* or below, so I claim one before
  the block empties.

Both are the same machinery with a different predicate, so they should share
one evaluation loop.

### Configure
Per location and per watch: radius, which operators count, scarcity threshold,
minimum remaining range. The operator set matters more than anything else — a
subscription only pays for one brand, so a block full of the other three is
still an empty block.

## Design decisions that matter

**Edge trigger, with a baseline.** When a watch is armed, record the set of
vehicle IDs currently inside the fence. Fire on `current − baseline`, never on
the count. Normally the baseline is empty (that is *why* you armed it), but
recording it anyway is what stops a watch armed at the wrong moment from firing
instantly.

**Fire once, then disarm.** A watch that keeps notifying is noise. Default:
first hit notifies and disarms. Offer "keep watching" for the case where you
missed it. Every watch also carries a **TTL** (default ~30 minutes) so nothing
polls forever.

**Poll no faster than 30 s.** Measured, not assumed — the upstream feed's
`last_updated` advances in exact 30-second steps (see README). Polling at 20 s
just reads the same snapshot twice, and it is rude to a free public dataset.

**One poll should serve many watches.** With a handful of watches, one query per
watch per tick is fine and simple. If that grows, coalesce nearby watches into a
single wider query and filter client-side — the geometry is cheap, the HTTP call
is not.

**Log every poll.** Cheap, and it answers a better question than any alarm does:
*when* does the block actually empty, and *when* does it restock? A week of
history tells you what time to leave, which beats being told you are too late.
The scheduler is already making the requests; persisting them costs nothing.

## Shape

A monorepo. Everything that is not personal lives here; coordinates,
credentials, thresholds and deployment details stay in gitignored `.env` files,
as they do today.

```
scootless/
  cmd/
    scootlessd/     backend daemon — HTTP API, poller, watch evaluation
  internal/
    entur/          upstream API client, operator IDs, vehicle model
    geo/            distance and bearing
    watch/          watch store, baseline, edge evaluation, TTL
    notify/         notification sinks behind one interface
    store/          SQLite — watches, config, poll history
  app/              the phone app
  docs/             this file and what follows it
  scootless.py      the original zero-dependency one-shot CLI
```

**Backend in Go.** Single static binary, trivial to drop on an always-on Linux
box, good at "one goroutine per watch plus an HTTP server". SQLite via a
pure-Go driver so there is no cgo and no build toolchain on the target.

**The Python CLI stays.** It is the published, dependency-free thing a stranger
can clone and run with no build step, and the README is written around that.
The cost is that the upstream API logic will exist twice, in Python and in Go.
That is a real cost and worth revisiting once the Go client is proven — either
retire the Python CLI, or have it call the backend when one is reachable.

**Notification is an interface, not a choice.** First sink is **MQTT**, because
it is a bus rather than a destination: the daemon publishes, and the last hop
stays undecided and swappable. Web Push, ntfy, Telegram and e-mail all become
subscribers rather than rewrites.

## Open questions

- **Push to a phone is the hard part, not the backend.** An installed PWA can
  receive Web Push on iOS 16.4+, but only once it has been added to the home
  screen, and the reliability bar for "wake me when a scooter appears" is high.
  A native build sidesteps it; a ready-made app (ntfy, Telegram) sidesteps it
  entirely at the cost of a third party. Worth deciding before the frontend is
  written, because it shapes it.
- **How often does an appearance actually happen?** The whole value of the watch
  feature rests on this and it is currently unmeasured. Cheap experiment: log
  appearances inside a realistic fence for a day.
- **Authentication.** The app must reach the backend from mobile data. A full
  interactive login every morning defeats the purpose of the feature; a
  long-lived per-device token probably does not.
- **One user or several?** Single-tenant is much simpler. Keying watches by
  device from the start makes multi-user a feature rather than a rewrite.

## Order of work

1. **Prove the data.** Confirm realtime access holds for all four operators,
   and measure appearance events inside a real fence. Everything downstream is
   worthless if appearances are rare.
2. **Backend.** Go daemon: upstream client, SQLite, HTTP API, config.
3. **Scheduler.** The 30 s poll loop, watch evaluation, TTL and disarm, history.
4. **Notifier.** MQTT publish, then pick the last hop.
5. **App.** Ask, arm a watch, manage locations and operator sets.
