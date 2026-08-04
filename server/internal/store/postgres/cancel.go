package postgres

import (
	"context"
	"time"

	"github.com/pushfree/pushfree/internal/receipts"
)

// CancelRepo is the Postgres implementation of receipts.CancelStore.
// It mirrors the SQLite CancelRepo's methods: GetReceipt loads the receipt
// snapshot for the cancel race-guard, and CancelPending atomically flips a
// pending receipt to canceled.
type CancelRepo struct{ db DB }

// NewCancelRepo constructs a CancelRepo over db.
func NewCancelRepo(db DB) *CancelRepo { return &CancelRepo{db: db} }

// GetReceipt loads the receipt's current lifecycle state and retry_count.
func (c *CancelRepo) GetReceipt(ctx context.Context, id string) (receipts.ReceiptSnapshot, error) {
	var state string
	var rc int
	err := c.db.QueryRowContext(ctx,
		`SELECT state, retry_count FROM receipts WHERE id = $1`, id).Scan(&state, &rc)
	if err != nil {
		return receipts.ReceiptSnapshot{}, mapErr(err)
	}
	return receipts.ReceiptSnapshot{State: receipts.State(state), RetryCount: rc}, nil
}

// CancelPending atomically transitions a pending receipt to the canceled
// terminal state. Returns true iff a row was updated.
func (c *CancelRepo) CancelPending(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := c.db.ExecContext(ctx,
		`UPDATE receipts SET state = 'canceled', canceled_at = $1 WHERE id = $2 AND state = 'pending'`,
		at, id)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, mapErr(err)
	}
	return n > 0, nil
}

// DeleteTimers removes every timer row tied to receiptID.
func (c *CancelRepo) DeleteTimers(ctx context.Context, receiptID string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM timers WHERE receipt_id = $1`, receiptID)
	return mapErr(err)
}

// ListCancellableByTag returns the ids of pending receipts with the given tag
// whose parent send belongs to appID.
func (c *CancelRepo) ListCancellableByTag(ctx context.Context, tag string, appID int64) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT r.id FROM receipts r
JOIN sends s ON r.send_id = s.id
WHERE r.tag = $1 AND r.state = 'pending' AND s.app_id = $2
ORDER BY r.id`, tag, appID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, mapErr(rows.Err())
}
