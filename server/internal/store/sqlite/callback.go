package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pushfree/pushfree/internal/store"
)

// CallbackRepo is the SQLite implementation of store.CallbackRepo. It also
// owns the dlq (dead-letter queue) table, since every dlq row is a child of a
// callback (dlq.callback_id REFERENCES callbacks(id)).
type CallbackRepo struct{ db DB }

func (c *CallbackRepo) Create(ctx context.Context, in *store.Callback) (int64, error) {
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO callbacks(receipt_id, url, state, next_attempt_at, attempts)
		 VALUES (?, ?, ?, ?, ?)`,
		in.ReceiptID, in.URL, in.State, nullTimePtr(in.NextAttemptAt), in.Attempts)
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("callback last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

func (c *CallbackRepo) GetByID(ctx context.Context, id int64) (store.Callback, error) {
	var (
		cb        store.Callback
		next      sql.NullString
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT id, receipt_id, url, state, next_attempt_at, attempts FROM callbacks WHERE id = ?`,
		id,
	).Scan(&cb.ID, &cb.ReceiptID, &cb.URL, &cb.State, &next, &cb.Attempts)
	if err != nil {
		return store.Callback{}, mapErr(err)
	}
	cb.NextAttemptAt = nullTime(next)
	return cb, nil
}

// CreateDLQ records a dead-letter entry for a permanently-failing callback.
func (c *CallbackRepo) CreateDLQ(ctx context.Context, in *store.DLQ) (int64, error) {
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO dlq(callback_id, last_error, at, attempts) VALUES (?, ?, ?, ?)`,
		in.CallbackID, in.LastError, rfc3339(in.At), in.Attempts)
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("dlq last insert id: %w", err)
	}
	in.ID = id
	return id, nil
}

// ListDLQForCallback returns the dead-letter history for a callback, oldest
// first.
func (c *CallbackRepo) ListDLQForCallback(ctx context.Context, callbackID int64) ([]store.DLQ, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT id, callback_id, last_error, at, attempts FROM dlq WHERE callback_id = ? ORDER BY id ASC`,
		callbackID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.DLQ
	for rows.Next() {
		var d store.DLQ
		var at sql.NullString
		if err := rows.Scan(&d.ID, &d.CallbackID, &d.LastError, &at, &d.Attempts); err != nil {
			return nil, mapErr(err)
		}
		if t, ok := parseTime(at.String, at.Valid); ok {
			d.At = t
		}
		out = append(out, d)
	}
	return out, mapErr(rows.Err())
}
