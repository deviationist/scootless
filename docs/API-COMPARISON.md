# Comparing the Entur and Ryde APIs during a real unlock

The purpose: put the fully public aggregator feed (Entur) beside the operator's
own app traffic (Ryde), captured over the same vehicle at the same moments, and
see exactly what each exposes — and what the operator's private API reveals that
the public one does not.

This extends the ride test in RIDE-TEST.md: same trip, but both data sources are
recorded, not just Entur.

## What we already know, going in

From a first capture (2026-08-25, map browsing only, no unlock):

- Ryde's app talks to `qw-test.ryde.vip`, form-encoded requests, mostly JSON
  responses. **Not certificate-pinned** — a trusted mitm CA decrypts it.
- **`getNearScootersNew`** (precise per-scooter positions) returns an
  application-layer **encrypted** blob (`key2`). Better than Entur, which serves
  the same data in clear.
- **`getComScooters`** returns **plaintext**, including `redScooters`: a list of
  **raw 15-digit IMEIs** (validated by Luhn + TAC). These are permanent hardware
  serials of the scooters' cellular modems.
- **`getComScooters` carries no session token** — it needs a signed request
  (`userDevice` + `timeSign` + an Authorization header) but no logged-in user.
  User-specific endpoints (`getUserBankCards`, `getUserAllUseLog`,
  `getLocusByUseId`, …) all correctly require the token.

Open questions this test should answer:

1. **Does the Entur UUID equal (or derive from) the IMEI?** The public feed's
   Ryde ids are structured — every one begins `ea`, two prefixes fleet-wide —
   and the rental_uris leak a `deviceIMEI=` parameter. If the Entur id is a
   transform of the IMEI, a permanent hardware serial is exposed on a fully
   public, key-less feed.
2. **What does Ryde send at the moment of unlock**, and does the vehicle's
   representation there tie to the same IMEI?
3. **How does each API reflect the rental** — Entur was assumed to drop the
   vehicle; does Ryde flag it, move it, or drop it too?

## Setup (both captures running at once)

**Ryde side — mitmproxy on xavi:**

```bash
export PATH="$HOME/.local/bin:$PATH"
mitmdump --mode regular@9080 --listen-host 0.0.0.0 \
  -w ~/.local/state/scootless/ride-ryde-$(date +%H%M).flows &
# open port 9080 to the LAN for the duration, then close it after:
sudo nft add rule inet filter input tcp dport 9080 ip saddr 192.168.3.0/24 \
  accept comment '"TEMP-mitmproxy-scootless"'
```

Phone: proxy `192.168.3.9:9080`, mitm CA already trusted from last time.

**Entur side — poll the exact vehicle on xavi:**

```bash
# once the vehicle is chosen, follow it through the whole trip
./bin/scootless-track -every 10s -for 1h --json 'YRY:Vehicle:...' \
  | tee ~/.local/state/scootless/ride-entur-$(date +%H%M).ndjson
```

Use a 10 s interval here, not 20 s, because the unlock and reappear transitions
are what matter and they need tighter resolution than the standing count does.

## Protocol

1. **Choose the vehicle** from Entur (`/api/v1/vehicles`), note its Entur id and
   its `deviceIMEI` from the rental_uri, and hand the rider its lat/lon.
2. **Start both captures.** Confirm Ryde flows are decrypting and the tracker is
   seeing the vehicle.
3. **Rider reaches the vehicle, reports "at it".**
4. **Rider unlocks, reports "unlocking now"** with the time.
   - Entur: expect the id to vanish within a tick or two.
   - Ryde: capture what the unlock call sends and returns.
5. **Rider rides, parks, locks, reports "locked and left".**
6. **Both sides watch for reappearance** and record the end position.

## What to line up afterward, from data only

- Entur `deviceIMEI` (from rental_uri)  ⟷  Ryde `redScooters` IMEI  ⟷  Entur
  UUID. Establish whether the three identify the same physical vehicle, and
  whether the UUID is derivable from the IMEI.
- Unlock timestamp  ⟷  Entur vanish time  ⟷  the Ryde unlock request. Measure
  the lag on each side.
- End position reported by the rider  ⟷  Entur reappear position  ⟷  Ryde.

## Handling rules (unchanged)

- Only the rider's own account and own phone.
- **Do not print account-endpoint contents** (`getUser*`, `getFineOrders`,
  `getLocusByUseId`, bank cards, drunk-detection) into any file or transcript.
  The classifier blocks this and it is right to. Analyse the *shape* of those if
  needed, never the values.
- Capture files hold financial and location data; they stay in
  `~/.local/state/scootless/`, gitignored, never committed.
- Close the firewall port and stop mitmdump when done.

## The disclosure angle

If the test confirms the Entur UUID is the IMEI (or trivially derived from it),
that is a permanent hardware identifier exposed on a public, unauthenticated
feed — a concrete, specific instance of a known weakness class in
shared-mobility data. Report to Ryde, and to Entur for the rental_uri leak.
Known-class, not a novel exploit; report it plainly, do not overclaim, and do
not publish specifics before the operators have had a chance to respond.
