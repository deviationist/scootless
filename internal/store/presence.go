package store

import (
	"context"
	"time"
)

// Sighting is one vehicle observed inside a fence.
type Sighting struct {
	VehicleID string
	Operator  string
}

// Arrival is a vehicle that appeared inside a fence, with the moment it did.
type Arrival struct {
	Sighting
	At time.Time
}

// DefaultStale is how long a vehicle may go unseen before a later sighting
// counts as a fresh arrival rather than a continuation.
//
// It exists because a vehicle can blink out of the feed for a tick and come
// back; without tolerance every blink would be reported as a new scooter
// arriving, which is precisely the false alarm that would make the watch
// feature untrustworthy.
const DefaultStale = 3 * time.Minute

// ObservePresence records the vehicles seen inside a fence at time at, and
// returns those that are newly arrived.
//
// A vehicle already present has its stint extended. A vehicle not seen within
// stale opens a new stint, which is what an arrival is.
func (s *Store) ObservePresence(ctx context.Context, fenceID string, at time.Time,
	seen []Sighting, stale time.Duration) ([]Arrival, error) {

	if stale <= 0 {
		stale = DefaultStale
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cutoff := at.Add(-stale).Unix()
	rows, err := tx.QueryContext(ctx, `
		SELECT vehicle_id, MAX(first_seen) FROM presence
		WHERE fence_id = ? AND last_seen >= ?
		GROUP BY vehicle_id`, fenceID, cutoff)
	if err != nil {
		return nil, err
	}
	open := make(map[string]int64)
	for rows.Next() {
		var id string
		var first int64
		if err := rows.Scan(&id, &first); err != nil {
			rows.Close()
			return nil, err
		}
		open[id] = first
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var arrivals []Arrival
	for _, sight := range seen {
		if first, ok := open[sight.VehicleID]; ok {
			if _, err := tx.ExecContext(ctx, `
				UPDATE presence SET last_seen = ?
				WHERE fence_id = ? AND vehicle_id = ? AND first_seen = ?`,
				at.Unix(), fenceID, sight.VehicleID, first); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO presence (fence_id, vehicle_id, operator, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(fence_id, vehicle_id, first_seen) DO UPDATE SET
			  last_seen = excluded.last_seen`,
			fenceID, sight.VehicleID, sight.Operator, at.Unix(), at.Unix()); err != nil {
			return nil, err
		}
		arrivals = append(arrivals, Arrival{Sighting: sight, At: at})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return arrivals, nil
}

// Arrivals returns the vehicles that arrived in a fence during [from, to).
func (s *Store) Arrivals(ctx context.Context, fenceID string, from, to time.Time) ([]Arrival, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT vehicle_id, operator, first_seen FROM presence
		WHERE fence_id = ? AND first_seen >= ? AND first_seen < ?
		ORDER BY first_seen`, fenceID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Arrival
	for rows.Next() {
		var a Arrival
		var at int64
		if err := rows.Scan(&a.VehicleID, &a.Operator, &at); err != nil {
			return nil, err
		}
		a.At = time.Unix(at, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// PresentIDs returns the vehicles currently considered inside a fence, which
// is what a newly armed watch records as its baseline.
func (s *Store) PresentIDs(ctx context.Context, fenceID string, at time.Time,
	stale time.Duration) ([]string, error) {

	if stale <= 0 {
		stale = DefaultStale
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT vehicle_id FROM presence
		WHERE fence_id = ? AND last_seen >= ?`, fenceID, at.Add(-stale).Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
