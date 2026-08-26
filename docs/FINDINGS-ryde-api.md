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
