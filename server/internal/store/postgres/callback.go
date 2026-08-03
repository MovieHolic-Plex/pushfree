package postgres

import (
	"context"
	"database/sql"

	"github.com/pushfree/pushfree/internal/store"
)

// CallbackRepo is the Postgres implementation of store.CallbackRepo. It also
// owns the dlq (dead-letter queue) table, since every dlq row is a child of a
// callback.
type CallbackRepo struct{ db DB }

// Create inserts a callback row and writes the assigned id back to in.ID.
func (c *CallbackRepo) Create(ctx context.Context, in *store.Callback) (int64, error) {
	err := c.db.QueryRowContext(ctx,
		`INSERT INTO callbacks(receipt_id, url, state, next_attempt_at, attempts)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		in.ReceiptID, in.URL, in.State, timeArg(in.NextAttemptAt), in.Attempts).
		Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

// GetByID loads a callback by id.
func (c *CallbackRepo) GetByID(ctx context.Context, id int64) (store.Callback, error) {
	var (
		cb   store.Callback
		next sql.NullTime
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT id, receipt_id, url, state, next_attempt_at, attempts FROM callbacks WHERE id = $1`,
		id,
	).Scan(&cb.ID, &cb.ReceiptID, &cb.URL, &cb.State, &next, &cb.Attempts)
	if err != nil {
		return store.Callback{}, mapErr(err)
	}
	cb.NextAttemptAt = nullTime(next)
	return cb, nil
}

// CreateDLQ records a dead-letter entry for a callback and writes the id back.
func (c *CallbackRepo) CreateDLQ(ctx context.Context, in *store.DLQ) (int64, error) {
	err := c.db.QueryRowContext(ctx,
		`INSERT INTO dlq(callback_id, last_error, at, attempts) VALUES ($1, $2, $3, $4) RETURNING id`,
		in.CallbackID, in.LastError, in.At, in.Attempts).Scan(&in.ID)
	if err != nil {
		return 0, mapErr(err)
	}
	return in.ID, nil
}

// ListDLQForCallback returns the dead-letter history for a callback, oldest
// first.
func (c *CallbackRepo) ListDLQForCallback(ctx context.Context, callbackID int64) ([]store.DLQ, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, callback_id, last_error, at, attempts FROM dlq WHERE callback_id = $1 ORDER BY id ASC`,
		callbackID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.DLQ
	for rows.Next() {
		var d store.DLQ
		var at sql.NullTime
		if err := rows.Scan(&d.ID, &d.CallbackID, &d.LastError, &at, &d.Attempts); err != nil {
			return nil, mapErr(err)
		}
		if at.Valid {
			d.At = at.Time
		}
		out = append(out, d)
	}
	return out, mapErr(rows.Err())
}
