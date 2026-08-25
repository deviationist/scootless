package store

// schema is applied on every Open. Every statement is idempotent, so opening
// an existing database is a no-op rather than a migration step.
const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS fence (
  id        TEXT PRIMARY KEY,
  name      TEXT NOT NULL,
  lat       REAL NOT NULL,
  lon       REAL NOT NULL,
  radius_m  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS watch (
  id           TEXT PRIMARY KEY,
  device       TEXT NOT NULL,
  kind         TEXT NOT NULL,
  fence_id     TEXT NOT NULL REFERENCES fence(id),
  operators    TEXT NOT NULL,
  min_range_m  INTEGER NOT NULL DEFAULT 0,
  threshold    INTEGER,
  baseline     TEXT NOT NULL,
  repeat       INTEGER NOT NULL DEFAULT 0,
  state        TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  fired_at     INTEGER
);

CREATE INDEX IF NOT EXISTS watch_state_idx ON watch(state, expires_at);

CREATE TABLE IF NOT EXISTS event (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  watch_id  TEXT NOT NULL REFERENCES watch(id),
  at        INTEGER NOT NULL,
  payload   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS event_watch_idx ON event(watch_id, at);

-- One row per fence per operator per tick: the cheap history that answers
-- when the block empties and when it refills.
CREATE TABLE IF NOT EXISTS sample (
  fence_id  TEXT NOT NULL REFERENCES fence(id),
  at        INTEGER NOT NULL,
  operator  TEXT NOT NULL,
  count     INTEGER NOT NULL,
  PRIMARY KEY (fence_id, at, operator)
);

CREATE INDEX IF NOT EXISTS sample_at_idx ON sample(fence_id, at);

-- One row per continuous stint a vehicle spends inside a fence. Far smaller
-- than storing every vehicle on every tick, and a row whose first_seen is
-- later than the fence's own start is exactly an arrival - the event the
-- watch feature sells.
CREATE TABLE IF NOT EXISTS presence (
  fence_id   TEXT NOT NULL REFERENCES fence(id),
  vehicle_id TEXT NOT NULL,
  operator   TEXT NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  PRIMARY KEY (fence_id, vehicle_id, first_seen)
);

-- How far away the nearest vehicle of each operator is, including when that
-- is outside the fence. A zero count answers "is there one here"; this
-- answers "and if not, how far do I have to walk".
CREATE TABLE IF NOT EXISTS nearest (
  fence_id   TEXT NOT NULL REFERENCES fence(id),
  at         INTEGER NOT NULL,
  operator   TEXT NOT NULL,
  distance_m INTEGER,          -- NULL: none found within reach
  vehicle_id TEXT,
  PRIMARY KEY (fence_id, at, operator)
);

CREATE INDEX IF NOT EXISTS nearest_at_idx ON nearest(fence_id, operator, at);

CREATE INDEX IF NOT EXISTS presence_open_idx ON presence(fence_id, last_seen);
CREATE INDEX IF NOT EXISTS presence_arrival_idx ON presence(fence_id, first_seen);
`
