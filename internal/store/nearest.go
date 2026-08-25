package store

import (
	"context"
	"database/sql"
	"time"
)

// Nearest is how far away the closest vehicle of one operator is. Distance is
// nil when the operator has nothing within the searched reach at all, which is
// a different answer from "it is far away" and worth keeping distinct.
type Nearest struct {
	Operator  string
	At        time.Time
	DistanceM *int
	VehicleID string
}

// RecordNearest stores one operator's nearest vehicle for a fence at a tick.
func (s *Store) RecordNearest(ctx context.Context, fenceID string, at time.Time, n Nearest) error {
	var dist any
	if n.DistanceM != nil {
		dist = *n.DistanceM
	}
	var vid any
	if n.VehicleID != "" {
		vid = n.VehicleID
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nearest (fence_id, at, operator, distance_m, vehicle_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(fence_id, at, operator) DO UPDATE SET
		  distance_m = excluded.distance_m, vehicle_id = excluded.vehicle_id`,
		fenceID, at.Unix(), n.Operator, dist, vid)
	return err
}

// LatestNearest returns the most recent nearest reading per operator.
func (s *Store) LatestNearest(ctx context.Context, fenceID string) (map[string]Nearest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.operator, n.at, n.distance_m, n.vehicle_id
		FROM nearest n
		JOIN (
		  SELECT operator, MAX(at) AS at FROM nearest
		  WHERE fence_id = ? GROUP BY operator
		) latest ON latest.operator = n.operator AND latest.at = n.at
		WHERE n.fence_id = ?`, fenceID, fenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]Nearest{}
	for rows.Next() {
		var (
			n    Nearest
			at   int64
			dist sql.NullInt64
			vid  sql.NullString
		)
		if err := rows.Scan(&n.Operator, &at, &dist, &vid); err != nil {
			return nil, err
		}
		n.At = time.Unix(at, 0).UTC()
		if dist.Valid {
			d := int(dist.Int64)
			n.DistanceM = &d
		}
		n.VehicleID = vid.String
		out[n.Operator] = n
	}
	return out, rows.Err()
}

// NearestHistory returns one operator's nearest-distance readings over a
// window, which is what shows a drought widening or easing.
func (s *Store) NearestHistory(ctx context.Context, fenceID, operator string,
	from, to time.Time) ([]Nearest, error) {

	rows, err := s.db.QueryContext(ctx, `
		SELECT operator, at, distance_m, vehicle_id FROM nearest
		WHERE fence_id = ? AND operator = ? AND at >= ? AND at < ?
		ORDER BY at`, fenceID, operator, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Nearest
	for rows.Next() {
		var (
			n    Nearest
			at   int64
			dist sql.NullInt64
			vid  sql.NullString
		)
		if err := rows.Scan(&n.Operator, &at, &dist, &vid); err != nil {
			return nil, err
		}
		n.At = time.Unix(at, 0).UTC()
		if dist.Valid {
			d := int(dist.Int64)
			n.DistanceM = &d
		}
		n.VehicleID = vid.String
		out = append(out, n)
	}
	return out, rows.Err()
}

// LatestCounts returns the most recent recorded count per operator for a
// fence - what is there right now, as of the last tick.
func (s *Store) LatestCounts(ctx context.Context, fenceID string) (map[string]int, time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(at) FROM sample WHERE fence_id = ?`, fenceID).Scan(&at)
	if err != nil || at == 0 {
		// No samples yet is not an error: the collector may have just started.
		return map[string]int{}, time.Time{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT operator, count FROM sample WHERE fence_id = ? AND at = ?`, fenceID, at)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var op string
		var n int
		if err := rows.Scan(&op, &n); err != nil {
			return nil, time.Time{}, err
		}
		out[op] = n
	}
	return out, time.Unix(at, 0).UTC(), rows.Err()
}
