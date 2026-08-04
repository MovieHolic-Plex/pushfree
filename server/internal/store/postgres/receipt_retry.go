package postgres

import (
	"context"
	"time"

	"github.com/pushfree/pushfree/internal/receipts"
)

// This file adds the receipts.RetryStore surface to *ReceiptRepo so the
// priority-2 retry scheduler (internal/timers/retry.go) can drive the
// retry/expire lifecycle against real Postgres receipt rows.
//
// Distinguished from the SQLite adapter (receipt_retry.go) only by the $N
// placeholder style; the conditional WHERE guards are identical.

// GetReceipt loads the receipt's current state and retry_count.
func (r *ReceiptRepo) GetReceipt(ctx context.Context, id string) (receipts.ReceiptSnapshot, error) {
	var state string
	var rc int
	err := r.db.QueryRowContext(ctx,
		`SELECT state, retry_count FROM receipts WHERE id = $1`, id).Scan(&state, &rc)
	if err != nil {
		return receipts.ReceiptSnapshot{}, mapErr(err)
	}
	return receipts.ReceiptSnapshot{State: receipts.State(state), RetryCount: rc}, nil
}

// IncrementRetry bumps retry_count by one and records the delivery attempt time.
func (r *ReceiptRepo) IncrementRetry(ctx context.Context, id string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE receipts SET retry_count = retry_count + 1, last_delivered_at = $1
		 WHERE id = $2 AND state IN ('pending','delivered')`,
		at, id); err != nil {
		return mapErr(err)
	}
	return nil
}

// SetExpired transitions the receipt to the expired terminal state.
func (r *ReceiptRepo) SetExpired(ctx context.Context, id string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE receipts SET state = 'expired', expired_at = $1
		 WHERE id = $2 AND state IN ('pending','delivered')`,
		at, id); err != nil {
		return mapErr(err)
	}
	return nil
}
