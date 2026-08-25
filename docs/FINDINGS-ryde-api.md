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
