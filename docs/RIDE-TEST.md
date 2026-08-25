# Ride test — protocol

A single-subject, self-consented experiment: one person rides one scooter and
reports what they did, while the feed is observed. It exists to establish four
things that have so far only been inferred.

## What it establishes

1. **Does an active rental remove a vehicle from the feed?** Everything built
   here assumes so — it is why `isReserved` is permanently false, and it is the
   basis for telling "ridden away" apart from "collected by a van". It has
   never been directly observed.
2. **Unlock-to-vanish latency.** How long after unlocking does the feed reflect
   it? This is the detection floor for *"somebody just took the last one"*.
3. **Park-to-reappear latency.** How long after locking does it become visible
   again? This bounds how quickly an appearance watch can fire on a returning
   scooter — the most common way one appears.
4. **Does the identifier survive the trip?** If the same vehicle returns under
   the same id, then trips can be linked to vehicles from public data by anyone.
   See *If the id persists* below.

## Preconditions

- **Between 05:00 and 23:00.** Rentals are banned overnight in Oslo, so nothing
  can be unlocked outside that window.
- The collector is running (`systemctl --user is-active scootlessd`).
- The chosen operator's feed is healthy — check `feed_dark` in
  `/api/v1/status` first. A test against a dark feed measures nothing.
- The trip stays inside the 300 m fence, so both ends are observed.

## Roles

- **Rider** — on the street, reporting events by timestamp.
- **Observer** — at the machine, running the tracker and recording.

## Protocol

**1. Pick the vehicle.** The observer lists candidates:

```bash
curl -s "localhost:8099/api/v1/vehicles?lat=<lat>&lon=<lon>&radius=300&operators=ryde" \
  | python3 -m json.tool
```

Choose one and hand the rider its **latitude and longitude** — not its id, which
is not written anywhere on the scooter. Position is the only way to identify a
specific vehicle in the physical world.

**2. Start the tracker** before the rider sets off:

```bash
./bin/scootless-track -every 20s -for 2h --json 'YRY:Vehicle:...' | tee ride-$(date +%H%M).ndjson
```

**3. The rider walks to it** and confirms arrival, so that walking time is not
confused with anything else.

**4. The rider reports "unlocking now"** — the timestamp matters more than the
words. The observer notes it to the second.

**5. Confirm identity by disappearance.** Whichever id vanishes within ~40 s of
that timestamp is the right vehicle. If the tracked id does *not* vanish, either
the wrong vehicle was chosen or assumption (1) is false — both are results, and
the second is the more interesting one.

**6. The rider rides, parks, locks, and reports "locked and left"**, again by
timestamp. Then walks home.

**7. Watch for the reappearance** and record where it lands.

## What to record

| | |
|---|---|
| `t_unlock` | rider's report |
| `t_vanish` | first tick with the id absent |
| `t_lock` | rider's report |
| `t_reappear` | first tick with the id present again |
| `id_before` / `id_after` | to see whether the identifier survived |
| position after | against where the rider believes they parked |

Latencies are only accurate to the poll interval, so `-every 20s` puts ±20 s on
every measurement. Use a shorter interval only for the vanish and reappear
windows, and not for hours — the data changes no faster than ~30 s.

## Reading the result

- **Vanishes on unlock, returns on park, same id** — every assumption holds, and
  ridden-vs-relocated classification is sound.
- **Never vanishes** — assumption (1) is wrong. Everything resting on it needs
  revisiting, including the departure analysis.
- **Returns under a different id** — cross-trip linkage is impossible, which is
  good for privacy and bad for the "same scooter came back" analysis.
- **Reappears far from where the rider parked** — the feed's positions cannot be
  trusted for walking directions, which matters for the notification's usefulness.

## If the id persists

A stable identifier that survives a rental means a public, key-less feed can be
used to link a vehicle's trips over time — origin and destination pairs, for any
vehicle, by anyone. Ryde's identifiers are additionally *structured* rather than
random: every one observed begins with the same two characters, and the fleet
shows only two distinct three-character prefixes, so they are derived from
something rather than generated randomly. Voi and Bolt use random identifiers.

If the test confirms persistence, the finding is worth reporting to the operator
before it is written up anywhere public, and to the data aggregator as well,
since the aggregation is what makes it convenient. This is a known weakness class
in shared-mobility feeds rather than a novel discovery, which is precisely why
some operators rotate identifiers.

The test itself involves one consenting participant tracking their own trip. It
should stay that way.
