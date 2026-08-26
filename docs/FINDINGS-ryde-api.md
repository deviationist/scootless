# Findings: Ryde app API (first capture, map browsing only)

Captured 2026-08-25 via mitmproxy on a consenting user's own phone and account,
browsing the Ryde map around Oslo. No unlock yet — that is the next test
(API-COMPARISON.md). Raw capture kept at `~/.local/state/scootless/*.flows`
(gitignored; contains the account holder's own financial and trip data — not
committed, not to be printed).

## Transport

- Host `qw-test.ryde.vip`, form-encoded request bodies, JSON responses.
- **No certificate pinning.** A trusted mitm CA decrypts everything; the app
  worked normally throughout.
- Requests are signed: every call carries `userDevice`, `timeSign` (an HMAC over
  the request), a `timestamp`, and an `Authorization` header.

## Two layers of response protection, applied unevenly

| Endpoint | Payload | Notes |
|---|---|---|
| `getNearScootersNew` | **encrypted** (`key2` blob) | precise per-scooter positions; better than Entur, which serves these in clear |
| `getComScooters` | **plaintext** | contains `redScooters`: raw IMEIs |
| `getFencesInfo`, `getCityFences`, `getQuPoints` | plaintext | parking / no-go zones, points |

## The identifiers

`getComScooters` → `redScooters` is a list of **158 strings**, of which **157
are valid 15-digit IMEIs** (Luhn checksum passes; the odd one is a 29-char
outlier). The Type Allocation Codes resolve to **5 device models** across the
fleet. An IMEI is the permanent, globally-unique serial of the cellular modem —
not a rotating or per-session value.

This connects to the public Entur feed:

- Entur's Ryde vehicle ids are **structured**, not random — every one observed
  begins `ea`, and the whole local fleet shows only two 3-char prefixes.
- Entur's Ryde `rental_uris` contain a `deviceIMEI=` query parameter.
- Ryde's own API traffics in **raw IMEIs**.

Hypothesis for the next test: the Entur UUID is the IMEI, hashed or lightly
transformed. If so, a permanent hardware identifier is exposed on a fully
public, key-less feed.

## Auth boundary

Session `token` is the real authentication. Four endpoints were observed with
**no token**: `getComScooters`, `getTimeService`, `getAppVersion`, `getAds`.
Three are trivial. The fourth — `getComScooters` — is the one leaking raw
IMEIs. So the raw-IMEI endpoint needs a *signed* request but **no logged-in
user**. All user-specific endpoints (`getUserBankCards`, `getUserAllUseLog`,
`getLocusByUseId`, `getUserDrunkInfoLast`, …) correctly require the token.

The auth boundary is in the right place for *account* data. It is the *fleet*
data — permanent hardware IDs — that is under-protected, gated only by the
request-signing scheme rather than by a login.

## Not yet established

- Whether the request signature (`timeSign`) is a real barrier or trivially
  reproduced from the client. Untested; do not assume either way.
- Whether the Entur UUID is provably the IMEI. Needs the unlock test.
- Whether IMEIs persist across days (expected, since IMEIs are permanent, but
  unobserved).

## Disposition

Known weakness class in shared-mobility feeds, not a novel exploit — which is
exactly why some operators rotate identifiers. Worth a plain-language report to
Ryde (raw IMEIs, unauthenticated endpoint) and to Entur (deviceIMEI in a public
rental_uri) once the unlock test confirms the id linkage. Do not publish
specifics before the operators can respond.

---

# Update: the identifier is the IMEI, and it is on the public feed

The earlier sections framed this around Ryde's app. The sharper finding is on
the **public** side, and it settles the question that actually matters —
*does a vehicle's identifier rotate across trips or across customers?*

## The answer: it cannot rotate

The identifier exposed for a Ryde vehicle is its **IMEI** — the cellular modem's
hardware serial. That fact does the work:

- **Cross-customer.** The Entur feed is public and account-less. The IMEI is
  therefore identical for every observer, by construction. There is no customer
  dimension for it to vary on.
- **Cross-trip / cross-day.** An IMEI is permanent by physical definition. It
  does not change when a rental ends or a new rider begins one.

The only residual assumption is that Ryde does not swap a vehicle's physical
modem or falsify the field — implausible, and the unlock test plus multi-day
collector data confirm it empirically by following one IMEI through a real
rental and across days.

## Evidence: the same IMEI in both sources, matched directly

Measured 2026-08-25, joining the app capture to the public feed:

- The public `rydeoslo` GBFS feed carried **4,882 vehicles, and every one
  (4,882 / 4,882) exposed a `deviceIMEI=` in its `rental_uris`.**
- **147 of the 157 IMEIs** seen in Ryde's app (`getComScooters` → `redScooters`)
  were present in that public feed, matched on the full 15-digit IMEI — not just
  a shared model prefix. The ~10 misses are consistent with normal feed churn in
  the minutes between the two captures.

So the same permanent hardware identifier appears in both the operator's app API
and the fully public aggregator feed.

## The UUID is a red herring (but probably also derived)

Entur's vehicle id is a UUIDv3 (deterministic, name-based) and is structured —
every observed Ryde id begins `ea`, with only two 3-char prefixes fleet-wide —
which is consistent with it being a hash of the IMEI under a private namespace
(common namespaces were tested and did not match; a private one cannot be
brute-forced, so this is suggestive, not proven). It does not matter either way:
the IMEI itself is in the feed in clear, so the UUID's derivation is irrelevant
to the exposure.

## What this is, stated precisely

> Entur's public, unauthenticated mobility feed exposes a permanent hardware
> identifier (the IMEI) for every Ryde scooter. Because an IMEI never changes,
> anyone can poll the feed and build a persistent movement history of any
> individual vehicle — origin/destination pairs, dwell times, restock patterns —
> with no account, no key, and no cooperation from the operator.

**The primary leak is on Entur's surface** (the `deviceIMEI` parameter inside the
public `rental_uris`), with Ryde as the upstream source of the data. A
disclosure therefore goes to both.

## The boundary that keeps this a privacy finding, not a how-to

The IMEI enables tracking **vehicles**, not **people**. Going from "vehicle X
moved A→B" to "person Y took that trip" requires linking the vehicle to a rider,
which needs data we do not have and must not obtain — a rider's account, a
survey tied to identity, or correlation against a person's known movements. The
finding stops at vehicle-level trackability from public data. It should stay
there.

---

# Update: the legal registration plate is NOT in the feed (checked)

Norwegian e-scooters carry a state registration plate (kjennemerke) — the test
vehicle's was `OSZ478`. An obvious question is whether that plate leaks into the
mobility API alongside the operator's own identifiers. It does not. Checked
across every surface (2026-08-26):

| Surface | Plate present? |
|---|---|
| Ryde app API capture | no — no plate value, no plate-shaped string |
| Ryde APK strings | no plate / registration / kjennemerke references |
| Entur `Vehicle` GraphQL type (14 fields) | no plate/registration field |
| Raw Ryde GBFS `free_bike_status` (9 fields) | no plate/registration field |

So a scooter carries **two independent identifier systems that do not join in
any remote data source**:

- **Operator system**, exposed in the API: the visible unlock number (`377489`)
  → the Entur UUID (`ea377489…`) → the IMEI (`864…`). This is what a feed
  logger sees and can track.
- **State system**, only on the physical plate: `OSZ478`, resolvable in Statens
  vegvesen's public vehicle register — but only by someone who has read the
  plate off the physical scooter.

This is a point in the design's favour, and worth stating so a reader does not
assume the worst: the trackable operator identifier and the government
registration are **siloed**. You cannot pivot from a feed-tracked scooter to its
legal plate, or vice versa, without being physically at the vehicle to read both.

It does not change the core finding — the operator system alone already gives
durable per-vehicle tracking from public data — but it bounds it: the leak is
operator-identifier trackability, not a bridge to the state vehicle register.

---

# Update: the ride test, and a correction — the full UUID rotates per trip

A consenting single-subject ride test (2026-08-26, the account holder's own
scooter and account) ran the vehicle through a full rental while observing the
public feed. It confirmed the core claim, corrected an overstatement, and
sharpened the mechanism.

## What the ride test showed

Baseline captured before unlock; scooter #377489 at `59.930690, 10.767192`,
IMEI `864…58`, full id `ea377489-65e1-3f09-8f10-098fb5cecedd`.

| Event | Observation |
|---|---|
| Remote unlock | Vanished from **all** public surfaces (GraphQL by-id, batch, spatial, raw GBFS) in ~18 s |
| Held (locked, rental running) | Stayed absent for 20+ min — "locked mid-ride" is invisible, same as riding |
| Ride ~2.8 km, end rental | Reappeared, but **minutes** later — end-ride latency ≫ unlock latency |
| Position on reappearance | `59.911440, 10.734196`, **2,822 m** from start — **confirmed accurate** against where the rider actually parked |
| IMEI on reappearance | `864…58` — **identical**. The permanent id survived the trip. |

So the feed's coordinates are trustworthy, and the IMEI is stable across a
rental — cross-trip tracking works, exactly as the IMEI (hardware serial)
predicted.

## The correction: the full UUID is per-trip

The full Entur UUID is **not** stable. Across the rental it changed:

```
before:  ea377489-65e1-3f09-8f10-098fb5cecedd
after:   ea377489-53dc-3307-8601-9ad33eb04466
         ^^^^^^^^  ------- rotating tail -------
         stable
```

The `ea` + scooter-number **prefix is stable**; the remaining tail is **minted
fresh per rental** and is unpredictable. This is a real, deliberate mitigation:
Ryde rotates the full identifier each trip, which defeats naive id-based
tracking.

It is undermined by two stable keys left in place:
- the **scooter number** embedded in the `ea377489…` prefix, and
- the permanent **IMEI** in the `rental_uris`.

## Correcting the "cheap targeted poll" claim

An earlier framing said a third party could track a scooter with one tiny
`vehicle(id:)` query per minute. **That is wrong**, and the ride test proved it:
the background poller was querying the *old* full id and never caught the
reappearance, because that id had ceased to exist. `vehicle(id:)` on the
pre-trip id returns null forever after the trip.

The actual tracking loop, corrected:

```
1. Scan the bulk GBFS feed (~2 MB / city)  ->  match on a STABLE key
                                               (number 377489, or IMEI 864…58)
2. Read the CURRENT full id from the match  ->  ea377489-53dc-… (valid this cycle)
3. (optional) cheap vehicle(id:) polls with that id — but only until the next
   rental rotates the tail, then the id is dead -> return to step 1.
```

So the load-bearing step is the **bulk-feed scan on a stable key**, not a cheap
targeted lookup. Ryde's rotation raises the attacker's *effort* (pull and grep a
city feed after each trip, rather than one small query) but not the *outcome*
(re-acquisition is guaranteed by the stable number and IMEI).

## Net

The rotation is genuine work that a stable prefix and a stable IMEI render
pointless. The clean fix is the same as before and now more clearly motivated:
rotate the **whole** identifier (including the prefix — do not embed the visible
number), and remove `deviceIMEI` from the public `rental_uris`. Either alone is
insufficient; both stable keys have to go.

---

# Update: the attack demonstrated end-to-end, autonomously

A second consenting ride (2026-08-26, the account holder's own scooter) ran the
*corrected* tracking method — matching on the stable number, not the rotating
full UUID — with no manual intervention.

Setup: a follower polling the whole-city GraphQL feed (30 km radius, gzip'd
~130 KB) every 8 s, filtering for the stable number `370341`.

Result:
- While rented, the scooter was absent from the feed (unlocatable), as before.
- On end-ride it reappeared, and the follower logged the parking position **on
  its own** — no cue from the rider. Position confirmed accurate against where
  the rider actually parked (a second confirmation of feed positional accuracy).
- End-ride → visible latency was seconds this time, versus minutes on the first
  ride: that latency is variable, not fixed.

This closes the loop the first ride left open. The complete chain, all from
public unauthenticated data:

```
number on the scooter (377489 / 370341)
   → stable feed key (the ea-prefix, or the IMEI)
   → poll the whole-city feed for that key
   → the scooter's position whenever it is parked, across every trip,
     forever, because the key never rotates.
```

The only per-trip-rotating identifier (the full UUID tail) is irrelevant: the
tracker never uses it. The rider's location is never needed either — only the
scooter's stable key.

## Boundary (unchanged, and load-bearing)

Every ride in these tests was the account holder tracking their own scooter,
with consent. The capability tracks *vehicles*, not *people*; going from a
vehicle trajectory to a named person needs external data (home→identity) that
these tests never touched and that a disclosure PoC must not build. The tooling
that demonstrates this stays a security proof-of-concept for the Ryde + Entur
disclosure. It is not a consumer feature: pointed at a scooter someone else is
riding, the identical code re-identifies a stranger's commute, which is the harm
being reported, not shipped.
