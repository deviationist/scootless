// Package store persists everything scootless needs to remember: the fences
// being watched, the watches themselves, and enough history to answer when a
// place empties and when it refills.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: no cgo, so the binary stays static

	"github.com/deviationist/scootless/internal/geo"
)

// Store is a handle on the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies the schema. Use
// ":memory:" for tests.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single writer avoids SQLITE_BUSY entirely: the poll loop and the HTTP
	// handlers share one process, and the write volume is a handful of rows
	// per tick.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("applying %q: %w", pragma, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for queries that do not warrant a method.
func (s *Store) DB() *sql.DB { return s.db }

// Fence is a place being watched: a point and a radius.
type Fence struct {
	ID      string
	Name    string
	At      geo.Point
	RadiusM int
}

// SaveFence inserts or updates a fence.
func (s *Store) SaveFence(ctx context.Context, f Fence) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fence (id, name, lat, lon, radius_m) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  name = excluded.name, lat = excluded.lat,
		  lon = excluded.lon, radius_m = excluded.radius_m`,
		f.ID, f.Name, f.At.Lat, f.At.Lon, f.RadiusM)
	return err
}

// Fences returns every fence, ordered by name.
func (s *Store) Fences(ctx context.Context) ([]Fence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, lat, lon, radius_m FROM fence ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Fence
	for rows.Next() {
		var f Fence
		if err := rows.Scan(&f.ID, &f.Name, &f.At.Lat, &f.At.Lon, &f.RadiusM); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Fence returns one fence by id.
func (s *Store) Fence(ctx context.Context, id string) (Fence, error) {
	var f Fence
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, lat, lon, radius_m FROM fence WHERE id = ?`, id).
		Scan(&f.ID, &f.Name, &f.At.Lat, &f.At.Lon, &f.RadiusM)
	return f, err
}

// RecordSample stores the count of one operator inside one fence at one tick.
// Re-recording the same tick is harmless.
func (s *Store) RecordSample(ctx context.Context, fenceID string, at time.Time,
	operator string, count int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sample (fence_id, at, operator, count) VALUES (?, ?, ?, ?)
		ON CONFLICT(fence_id, at, operator) DO UPDATE SET count = excluded.count`,
		fenceID, at.Unix(), operator, count)
	return err
}

// SamplePoint is one recorded count.
type SamplePoint struct {
	At       time.Time
	Operator string
	Count    int
}

// Samples returns the recorded counts for a fence in [from, to).
func (s *Store) Samples(ctx context.Context, fenceID string, from, to time.Time) ([]SamplePoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT at, operator, count FROM sample
		WHERE fence_id = ? AND at >= ? AND at < ?
		ORDER BY at, operator`, fenceID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SamplePoint
	for rows.Next() {
		var p SamplePoint
		var at int64
		if err := rows.Scan(&at, &p.Operator, &p.Count); err != nil {
			return nil, err
		}
		p.At = time.Unix(at, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// jsonStrings encodes a string slice for a TEXT column, never as SQL NULL.
func jsonStrings(v []string) (string, error) {
	if v == nil {
		v = []string{}
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func parseStrings(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}
