package store

import (
	"context"
	"errors"
)

// ErrFenceInUse reports a fence that an armed watch still depends on.
var ErrFenceInUse = errors.New("fence has armed watches")

// DeleteFence removes a fence and the history recorded against it.
//
// It refuses while an armed watch still references the fence. The poller
// iterates the fences that exist, so deleting one out from under a watch does
// not fail loudly — the watch simply sits armed and never fires again, which is
// the worst shape a failure can take here: somebody waiting for a notification
// that cannot arrive.
//
// The history goes with it, in the same transaction. sample, presence and
// nearest are all keyed on the fence, and a stable id is meant to be reused —
// a consumer moves one fence rather than making a second — so rows left behind
// would later be read as a new fence's own past.
//
// The referential checks are made here rather than left to the foreign keys.
// PRAGMA foreign_keys is per-connection and the pool does not set it, so the
// REFERENCES clauses in the schema are documentation on most connections rather
// than enforcement.
//
// Reports whether a fence was actually removed, so a caller can tell a deletion
// from a no-op.
func (s *Store) DeleteFence(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var armed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM watch WHERE fence_id = ? AND state = ?`,
		id, string(StateArmed)).Scan(&armed); err != nil {
		return false, err
	}
	if armed > 0 {
		return false, ErrFenceInUse
	}

	for _, q := range []string{
		`DELETE FROM watch WHERE fence_id = ?`,
		`DELETE FROM sample WHERE fence_id = ?`,
		`DELETE FROM presence WHERE fence_id = ?`,
		`DELETE FROM nearest WHERE fence_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return false, err
		}
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM fence WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}
