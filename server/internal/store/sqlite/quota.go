package sqlite

import (
	"context"

	"github.com/pushfree/pushfree/internal/store"
)

// QuotaRepo is the SQLite implementation of store.QuotaRepo. The monthly
// counter is an upsert keyed by (user_id, "YYYY-MM"); Increment is atomic so
// concurrent ingest calls cannot lose increments.
type QuotaRepo struct{ db DB }

// Increment atomically adds delta to the (userID, period) counter, creating
// the row if absent, and returns the post-increment count.
func (q *QuotaRepo) Increment(ctx context.Context, userID int64, period string, delta int64) (int64, error) {
	var after int64
	err := inTx(ctx, q.db, func(qe queryExec) error {
		if _, err := qe.ExecContext(ctx,
			`INSERT INTO quota_counters(user_id, period, count) VALUES (?, ?, ?)
			 ON CONFLICT(user_id, period) DO UPDATE SET count = count + excluded.count`,
			userID, period, delta); err != nil {
			return err
		}
		return qe.QueryRowContext(ctx,
			`SELECT count FROM quota_counters WHERE user_id = ? AND period = ?`,
			userID, period).Scan(&after)
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return after, nil
}

// Get returns the counter for (userID, period). A never-touched period yields
// a zero-count QuotaCounter rather than an error.
func (q *QuotaRepo) Get(ctx context.Context, userID int64, period string) (store.QuotaCounter, error) {
	var c store.QuotaCounter
	c.UserID = userID
	c.Period = period
	row := q.db.QueryRowContext(ctx,
		`SELECT count FROM quota_counters WHERE user_id = ? AND period = ?`, userID, period)
	if err := row.Scan(&c.Count); err != nil {
		if isNotFound(err) {
			return c, nil // zero count, not an error
		}
		return store.QuotaCounter{}, mapErr(err)
	}
	return c, nil
}

// isNotFound reports whether err is sql.ErrNoRows after mapErr normalization.
// mapErr converts sql.ErrNoRows -> store.ErrNotFound, so accept either.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return err == store.ErrNotFound || err.Error() == "sql: no rows in result set"
}

