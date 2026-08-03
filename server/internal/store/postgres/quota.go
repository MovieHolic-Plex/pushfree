package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pushfree/pushfree/internal/store"
)

// QuotaRepo is the Postgres implementation of store.QuotaRepo. The monthly
// counter is an upsert keyed by (user_id, "YYYY-MM"); Increment is atomic so
// concurrent ingest calls cannot lose increments.
type QuotaRepo struct{ db DB }

// Increment atomically adds delta to the (userID, period) counter, creating
// the row if absent, and returns the post-increment count.
func (q *QuotaRepo) Increment(ctx context.Context, userID int64, period string, delta int64) (int64, error) {
	var after int64
	err := inTx(ctx, q.db, func(qe queryExec) error {
		if _, err := qe.ExecContext(ctx,
			`INSERT INTO quota_counters(user_id, period, count) VALUES ($1, $2, $3)
			 ON CONFLICT(user_id, period) DO UPDATE SET count = quota_counters.count + EXCLUDED.count`,
			userID, period, delta); err != nil {
			return err
		}
		return qe.QueryRowContext(ctx,
			`SELECT count FROM quota_counters WHERE user_id = $1 AND period = $2`,
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
		`SELECT count FROM quota_counters WHERE user_id = $1 AND period = $2`, userID, period)
	if err := row.Scan(&c.Count); err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
			return c, nil // zero count, not an error
		}
		return store.QuotaCounter{}, mapErr(err)
	}
	return c, nil
}
