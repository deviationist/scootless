package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Kind distinguishes the two predicates a watch can hold.
type Kind string

const (
	// KindAppearance fires when a vehicle shows up that was not there when
	// the watch was armed. This is the "I need a scooter" case.
	KindAppearance Kind = "appearance"
	// KindScarcity fires when the count falls to a threshold or below.
	KindScarcity Kind = "scarcity"
)

// State is where a watch is in its lifecycle.
type State string

const (
	StateArmed     State = "armed"
	StateFired     State = "fired"
	StateExpired   State = "expired"
	StateCancelled State = "cancelled"
)

// ErrNotFound is returned when no watch has the given id.
var ErrNotFound = errors.New("not found")

// Watch is an armed request to be told when something changes.
type Watch struct {
	ID           string
	Device       string
	Kind         Kind
	FenceID      string
	OperatorKeys []string
	MinRangeM    int
	Threshold    int

	// Baseline is the set of vehicle ids present when the watch was armed.
	// An appearance watch fires on what is not in here - never on a count -
	// so that arming at an unlucky moment cannot fire instantly.
	Baseline []string

	// Repeat keeps the watch armed after it fires. Off by default: a watch
	// that keeps notifying is noise.
	Repeat bool

	State     State
	CreatedAt time.Time
	ExpiresAt time.Time
	FiredAt   *time.Time
}

// CreateWatch stores a new watch.
func (s *Store) CreateWatch(ctx context.Context, w *Watch) error {
	ops, err := jsonStrings(w.OperatorKeys)
	if err != nil {
		return err
	}
	base, err := jsonStrings(w.Baseline)
	if err != nil {
		return err
	}
	if w.State == "" {
		w.State = StateArmed
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO watch (id, device, kind, fence_id, operators, min_range_m,
		                   threshold, baseline, repeat, state, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Device, string(w.Kind), w.FenceID, ops, w.MinRangeM,
		w.Threshold, base, boolToInt(w.Repeat), string(w.State),
		w.CreatedAt.Unix(), w.ExpiresAt.Unix())
	return err
}

// Watch returns one watch by id.
func (s *Store) Watch(ctx context.Context, id string) (*Watch, error) {
	row := s.db.QueryRowContext(ctx, watchSelect+` WHERE id = ?`, id)
	w, err := scanWatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// ArmedWatches returns every watch still armed and not yet expired.
func (s *Store) ArmedWatches(ctx context.Context, now time.Time) ([]*Watch, error) {
	rows, err := s.db.QueryContext(ctx,
		watchSelect+` WHERE state = ? AND expires_at > ? ORDER BY created_at`,
		string(StateArmed), now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MarkFired records that a watch matched. A non-repeating watch is disarmed in
// the same statement, so a concurrent tick cannot fire it twice.
func (s *Store) MarkFired(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE watch
		SET fired_at = ?,
		    state = CASE WHEN repeat = 1 THEN state ELSE ? END
		WHERE id = ? AND state = ?`,
		at.Unix(), string(StateFired), id, string(StateArmed))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetState moves a watch to a terminal state.
func (s *Store) SetState(ctx context.Context, id string, state State) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE watch SET state = ? WHERE id = ?`, string(state), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpireWatches retires every armed watch past its deadline, and reports how
// many. No watch polls forever.
func (s *Store) ExpireWatches(ctx context.Context, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE watch SET state = ? WHERE state = ? AND expires_at <= ?`,
		string(StateExpired), string(StateArmed), now.Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RecordEvent stores what a watch reported, so a fire can be inspected later.
func (s *Store) RecordEvent(ctx context.Context, watchID string, at time.Time, payload []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO event (watch_id, at, payload) VALUES (?, ?, ?)`,
		watchID, at.Unix(), string(payload))
	return err
}

// Event is one notification a watch produced.
type Event struct {
	ID      int64
	WatchID string
	At      time.Time
	Payload []byte
}

// Events returns what a watch has reported, oldest first.
func (s *Store) Events(ctx context.Context, watchID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, watch_id, at, payload FROM event WHERE watch_id = ? ORDER BY at`, watchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		var payload string
		if err := rows.Scan(&e.ID, &e.WatchID, &at, &payload); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0).UTC()
		e.Payload = []byte(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

const watchSelect = `
	SELECT id, device, kind, fence_id, operators, min_range_m, threshold,
	       baseline, repeat, state, created_at, expires_at, fired_at
	FROM watch`

type scanner interface {
	Scan(dest ...any) error
}

func scanWatch(sc scanner) (*Watch, error) {
	var (
		w         Watch
		kind      string
		state     string
		ops       string
		base      string
		repeat    int
		threshold sql.NullInt64
		created   int64
		expires   int64
		fired     sql.NullInt64
	)
	if err := sc.Scan(&w.ID, &w.Device, &kind, &w.FenceID, &ops, &w.MinRangeM,
		&threshold, &base, &repeat, &state, &created, &expires, &fired); err != nil {
		return nil, err
	}
	w.Kind = Kind(kind)
	w.State = State(state)
	w.Repeat = repeat != 0
	w.Threshold = int(threshold.Int64)
	w.CreatedAt = time.Unix(created, 0).UTC()
	w.ExpiresAt = time.Unix(expires, 0).UTC()
	if fired.Valid {
		t := time.Unix(fired.Int64, 0).UTC()
		w.FiredAt = &t
	}
	var err error
	if w.OperatorKeys, err = parseStrings(ops); err != nil {
		return nil, err
	}
	if w.Baseline, err = parseStrings(base); err != nil {
		return nil, err
	}
	return &w, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CancelArmed cancels every armed watch, optionally limited to one device.
// It returns how many were cancelled — this is the "stop it" action, which
// should not require the caller to have kept a watch id around.
func (s *Store) CancelArmed(ctx context.Context, device string, now time.Time) (int, error) {
	var (
		res interface {
			RowsAffected() (int64, error)
		}
		err error
	)
	if device == "" {
		res, err = s.db.ExecContext(ctx,
			`UPDATE watch SET state = ? WHERE state = ?`,
			string(StateCancelled), string(StateArmed))
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE watch SET state = ? WHERE state = ? AND device = ?`,
			string(StateCancelled), string(StateArmed), device)
	}
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}
