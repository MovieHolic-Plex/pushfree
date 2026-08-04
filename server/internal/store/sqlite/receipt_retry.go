package sqlite

import (
	"context"
	"time"

	"github.com/pushfree/pushfree/internal/receipts"
)

// This file adds the receipts.RetryStore surface to *ReceiptRepo so the
// priority-2 retry scheduler (internal/timers/retry.go) can drive the
// retry/expire lifecycle against real receipt rows.
//
// The SQL mirrors the conditional-UPDATE shapes used in receipt_ack.go and
// receipt_gc.go. The WHERE state IN ('pending','delivered') guard on SetExpired
// prevents an illegal forward transition on an already-terminal receipt
// (acknowledged/canceled), matching the state machine in
// internal/receipts/statemachine.go.

// GetReceipt loads the receipt's current state and retry_count for the
// scheduler. An unknown receipt id yields store.ErrNotFound.
func (r *ReceiptRepo) GetReceipt(ctx context.Context, id string) (receipts.ReceiptSnapshot, error) {
	var state string
	var rc int
	err := r.db.QueryRowContext(ctx,
		`SELECT state, retry_count FROM receipts WHERE id = ?`, id).Scan(&state, &rc)
	if err != nil {
		return receipts.ReceiptSnapshot{}, mapErr(err)
	}
	return receipts.ReceiptSnapshot{State: receipts.State(state), RetryCount: rc}, nil
}

// IncrementRetry bumps retry_count by one. The `at` parameter records when
// the attempt occurred (stored as last_delivered_at so receipt polling shows
// the most recent delivery attempt).
func (r *ReceiptRepo) IncrementRetry(ctx context.Context, id string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE receipts SET retry_count = retry_count + 1, last_delivered_at = ?
		 WHERE id = ? AND state IN ('pending','delivered')`,
		rfc3339(at), id); err != nil {
		return mapErr(err)
	}
	return nil
}

// SetExpired transitions the receipt to the expired terminal state. The
// conditional WHERE prevents an illegal forward transition on an already-
// terminal row (idempotent no-op on acknowledged/canceled receipts).
func (r *ReceiptRepo) SetExpired(ctx context.Context, id string, at time.Time) error {
	if _, err := r.db.ExecContext(ctx,
		`UPDATE receipts SET state = 'expired', expired_at = ?
		 WHERE id = ? AND state IN ('pending','delivered')`,
		rfc3339(at), id); err != nil {
		return mapErr(err)
	}
	return nil
}
