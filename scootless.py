#!/usr/bin/env python3
"""scootless - how many rentable scooters are within reach, right now.

Answers the question you have before you put your shoes on: is there anything
out there, or am I walking? Covers Ryde, Voi, Bolt and Dott.

Data comes from Entur's national mobility API, which aggregates the GBFS feeds
Norwegian scooter operators are required to publish. No auth, no API key.
"""
import argparse
import json
import math
import os
import sys
import urllib.error
import urllib.request

API = "https://api.entur.io/mobility/v2/graphql"
DEFAULT_CLIENT_NAME = "scootless"
CONFIG = os.path.expanduser("~/.config/scootless/config.json")
ENV_FILE = ".env"

OPERATORS = {
    "ryde": "YRY:Operator:Ryde",
    "voi": "YVO:Operator:voi",
    "bolt": "YBO:Operator:bolt",
    "dott": "YDT:Operator:dott",
}

# The API returns at most this many; hitting it exactly means the list was cut short.
MAX_RESULTS = 500

QUERY = """
query ($lat: Float!, $lon: Float!, $range: Int!, $operators: [String], $count: Int!) {
  vehicles(lat: $lat, lon: $lon, range: $range, operators: $operators, count: $count) {
    id
    lat
    lon
    isReserved
    isDisabled
    currentRangeMeters
    currentFuelPercent
    rentalUris { android ios }
    system { operator { name { translation { value } } } }
  }
}
"""


def load_dotenv():
    """Read .env from the working dir or next to this script. No dependencies.

    Keeps personal values - home coordinates, your API client name - out of the
    repository. Nothing here is a secret, but a home address is still yours.
    """
    found = {}
    here = os.path.dirname(os.path.abspath(__file__))
    for path in (os.path.join(here, ENV_FILE), ENV_FILE):
        try:
            with open(path) as fh:
                lines = fh.readlines()
        except OSError:
            continue
        for line in lines:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, val = line.partition("=")
            found[key.strip()] = val.strip().strip("'\"")
    found.update({k: v for k, v in os.environ.items() if k.startswith("SCOOTLESS_")})
    return found


def env_defaults(env, cfg):
    """Fold SCOOTLESS_* values over the config file. Real args still win."""
    merged = dict(cfg)
    mapping = {
        "SCOOTLESS_RADIUS": ("radius", int),
        "SCOOTLESS_THRESHOLD": ("threshold", int),
        "SCOOTLESS_OPERATOR": ("operator", str),
        "SCOOTLESS_CLIENT_NAME": ("client_name", str),
    }
    for key, (name, cast) in mapping.items():
        if env.get(key):
            try:
                merged[name] = cast(env[key])
            except ValueError:
                print(f"scootless: ignoring bad {key}={env[key]!r}",
                      file=sys.stderr)
    if env.get("SCOOTLESS_LAT") and env.get("SCOOTLESS_LON"):
        try:
            merged.setdefault("locations", {})["home"] = [
                float(env["SCOOTLESS_LAT"]), float(env["SCOOTLESS_LON"])]
            merged["default_location"] = "home"
        except ValueError:
            print("scootless: ignoring bad SCOOTLESS_LAT/LON", file=sys.stderr)
    return merged


def load_config():
    try:
        with open(CONFIG) as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return {}


def save_config(cfg):
    os.makedirs(os.path.dirname(CONFIG), exist_ok=True)
    with open(CONFIG, "w") as fh:
        json.dump(cfg, fh, indent=2)
        fh.write("\n")


def haversine(lat1, lon1, lat2, lon2):
    """Great-circle distance in metres."""
    r = 6371008.8
    p1, p2 = math.radians(lat1), math.radians(lat2)
    dp = p2 - p1
    dl = math.radians(lon2 - lon1)
    a = math.sin(dp / 2) ** 2 + math.cos(p1) * math.cos(p2) * math.sin(dl / 2) ** 2
    return 2 * r * math.asin(math.sqrt(a))


ARROWS = ["N ^", "NE/", "E >", "SE\\", "S v", "SW/", "W <", "NW\\"]


def bearing(lat1, lon1, lat2, lon2):
    """Compass label for the direction you'd walk, e.g. 'NE/'."""
    p1, p2 = math.radians(lat1), math.radians(lat2)
    dl = math.radians(lon2 - lon1)
    y = math.sin(dl) * math.cos(p2)
    x = math.cos(p1) * math.sin(p2) - math.sin(p1) * math.cos(p2) * math.cos(dl)
    deg = (math.degrees(math.atan2(y, x)) + 360) % 360
    return ARROWS[int((deg + 22.5) % 360 // 45)]


def fetch(lat, lon, radius, operators, client_name):
    body = json.dumps({
        "query": QUERY,
        "variables": {"lat": lat, "lon": lon, "range": radius,
                      "operators": operators, "count": MAX_RESULTS},
    }).encode()
    req = urllib.request.Request(
        API, data=body,
        headers={"Content-Type": "application/json",
                 "ET-Client-Name": client_name},
    )
    with urllib.request.urlopen(req, timeout=25) as resp:
        payload = json.load(resp)
    if payload.get("errors"):
        raise RuntimeError(payload["errors"][0].get("message", "unknown API error"))
    return payload["data"]["vehicles"] or []


def operator_of(v):
    try:
        return v["system"]["operator"]["name"]["translation"][0]["value"]
    except (KeyError, IndexError, TypeError):
        return "?"


def collect(lat, lon, radius, operators, min_km, client_name):
    """Rentable vehicles only, nearest first, with distance/bearing attached."""
    out = []
    raw = fetch(lat, lon, radius, operators, client_name)
    for v in raw:
        # The feed lists vehicles the app won't actually rent you.
        if v.get("isDisabled") or v.get("isReserved"):
            continue
        km = (v.get("currentRangeMeters") or 0) / 1000
        if km < min_km:
            continue
        # currentFuelPercent is a 0-1 fraction, not a percentage. Ryde never
        # populates it at all, so range_km is the only battery signal there.
        pct = v.get("currentFuelPercent")
        pct = pct * 100 if pct is not None else None
        dist = haversine(lat, lon, v["lat"], v["lon"])
        if dist > radius:  # belt and braces; the API filters server-side too
            continue
        out.append({
            "id": v["id"],
            "lat": v["lat"],
            "lon": v["lon"],
            "distance_m": round(dist),
            "bearing": bearing(lat, lon, v["lat"], v["lon"]),
            "range_km": round(km, 1),
            "battery_pct": pct,
            "operator": operator_of(v),
            "app_link": (v.get("rentalUris") or {}).get("ios"),
        })
    out.sort(key=lambda x: x["distance_m"])
    return out, len(raw) >= MAX_RESULTS


def parse_coords(text):
    parts = text.replace(",", " ").split()
    if len(parts) != 2:
        raise ValueError("expected 'lat,lon'")
    return float(parts[0]), float(parts[1])


def resolve_place(args, cfg):
    """Where are we standing? --lat/--lon wins, then --at, then the default."""
    if args.lat is not None and args.lon is not None:
        return args.lat, args.lon, "given coordinates"
    locations = cfg.get("locations", {})
    name = args.at or cfg.get("default_location")
    if name and name in locations:
        return locations[name][0], locations[name][1], name
    return None, None, None


def main():
    env = load_dotenv()
    cfg = env_defaults(env, load_config())
    client_name = cfg.get("client_name", DEFAULT_CLIENT_NAME)
    p = argparse.ArgumentParser(
        prog="scootless",
        description="Rentable scooters within reach, right now.")
    p.add_argument("--at", metavar="NAME", help="use a saved location")
    p.add_argument("--lat", type=float)
    p.add_argument("--lon", type=float)
    p.add_argument("-r", "--radius", type=int, default=cfg.get("radius", 100),
                   metavar="M", help="search radius in metres (default: %(default)s)")
    p.add_argument("-t", "--threshold", type=int, default=cfg.get("threshold", 3),
                   metavar="N",
                   help="warn at or below this many (default: %(default)s)")
    p.add_argument("-o", "--operator", default=cfg.get("operator", "all"),
                   help="ryde, voi, bolt, dott, or a comma-separated subset "
                        "(default: %(default)s)")
    p.add_argument("--min-battery-km", type=float, default=0, metavar="KM",
                   help="ignore scooters with less range than this")
    p.add_argument("-n", "--limit", type=int, default=10, metavar="N",
                   help="how many to list (default: %(default)s)")
    p.add_argument("--json", action="store_true", help="machine-readable output")
    p.add_argument("--quiet", action="store_true",
                   help="print only the count; exit code carries the verdict")
    p.add_argument("--save-location", nargs=2, metavar=("NAME", "LAT,LON"),
                   help="store a location and make it the default")
    args = p.parse_args()

    if args.save_location:
        name, coords = args.save_location
        try:
            lat, lon = parse_coords(coords)
        except ValueError as exc:
            p.error(f"--save-location: {exc}")
        cfg.setdefault("locations", {})[name] = [lat, lon]
        cfg.setdefault("default_location", name)
        save_config(cfg)
        print(f"Saved '{name}' at {lat}, {lon} -> {CONFIG}")
        return 0

    lat, lon, place = resolve_place(args, cfg)
    if lat is None:
        p.error("no location. Pass --lat/--lon, or save one:\n"
                "  scootless --save-location home 59.9139,10.7522")

    if args.operator == "all":
        operators = list(OPERATORS.values())
    else:
        keys = [k.strip().lower() for k in args.operator.split(",")]
        unknown = [k for k in keys if k not in OPERATORS]
        if unknown:
            p.error(f"unknown operator(s): {', '.join(unknown)}. "
                    f"Choose from: {', '.join(OPERATORS)}, all")
        operators = [OPERATORS[k] for k in keys]

    try:
        found, truncated = collect(lat, lon, args.radius, operators,
                                   args.min_battery_km, client_name)
    except (urllib.error.URLError, RuntimeError, ValueError) as exc:
        print(f"scootless: could not reach the mobility API: {exc}",
              file=sys.stderr)
        return 2

    count = len(found)
    # A truncated list can only mean "lots", which is never a scarcity alarm.
    scarce = count <= args.threshold and not truncated
    shown = f"{count}+" if truncated else str(count)

    if args.json:
        print(json.dumps({
            "location": place, "lat": lat, "lon": lon,
            "radius_m": args.radius, "operator": args.operator,
            "count": count, "truncated": truncated,
            "threshold": args.threshold, "scarce": scarce,
            "vehicles": found[:args.limit],
        }, indent=2))
    elif args.quiet:
        print(shown)
    else:
        label = "scooters" if args.operator == "all" else args.operator.title()
        print(f"{shown} rentable {label} within {args.radius} m of {place}")
        if not count:
            print("  You are scootless. Widen the radius with -r, "
                  "or try -o all.")
        elif scarce:
            print(f"  SCARCE - at or below your threshold of {args.threshold}. "
                  f"Grab one before it's gone.")
        for v in found[:args.limit]:
            pct = f" {v['battery_pct']:.0f}%" if v["battery_pct"] is not None else ""
            print(f"  {v['distance_m']:4d} m {v['bearing']}  "
                  f"{v['range_km']:5.1f} km{pct}  {v['operator']}")
        if count > args.limit:
            print(f"  ... and {count - args.limit} more")

    # 0 = plenty, 10 = scarce (threshold hit), 11 = none at all.
    return 11 if count == 0 else (10 if scarce else 0)


if __name__ == "__main__":
    sys.exit(main())
