package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pushfree/pushfree/internal/receipts"
)

// This file is the SQLite implementation of receipts.CancelStore (todo 24). It
// is a NEW file, deliberately NOT an edit to worker-owned receipt.go/timer.go:
// the cancel endpoint's persistence surface is isolated here so it composes
// with concurrent work on the receipt lifecycle (todo 23) and the timer engine
// (todo 22) without touching those files. It reuses the package-local helpers
// (rfc3339, mapErr) but owns no shared state.
//
// Layering note: the driver (this package) implements a domain interface
// (receipts.CancelStore). That is dependency inversion done right -- the
// concrete driver depends on the domain abstraction, not the reverse -- and it
// keeps the cancel surface out of the shared store.go interface file (which a
// concurrent worker is editing), so this file commits cleanly on its own.

// CancelRepo is the SQLite implementation of receipts.CancelStore. It wraps a
// single *sql.DB (the same pool the rest of the store uses) and is constructed
// by NewCancelRepo at server-wiring time.
type CancelRepo struct{ db *sql.DB }

// NewCancelRepo builds a CancelRepo over an already-open *sql.DB. The caller
// owns the connection lifecycle (the *sqlite.Store outlives the repo). It is
// exported so server wiring (cmd/pushfree) and tests can construct it without
// depending on the unexported DB interface.
func NewCancelRepo(db *sql.DB) *CancelRepo { return &CancelRepo{db: db} }

// GetReceipt loads the receipt's lifecycle state and retry_count. An unknown
// id surfaces as store.ErrNotFound so the API layer can map it to 404.
func (c *CancelRepo) GetReceipt(ctx context.Context, id string) (receipts.ReceiptSnapshot, error) {
	var (
		state string
		rc    int
	)
	err := c.db.QueryRowContext(ctx,
		`SELECT state, retry_count FROM receipts WHERE id = ?`, id).Scan(&state, &rc)
	if err != nil {
		return receipts.ReceiptSnapshot{}, mapErr(err)
	}
	return receipts.ReceiptSnapshot{State: receipts.State(state), RetryCount: rc}, nil
}

// CancelPending atomically transitions a pending receipt to the canceled
// terminal state, recording canceled_at = at. The WHERE state = 'pending'
// guard makes it a no-op on any receipt that has already progressed (so it is
// the race guard and the "already canceled / delivered / acked" detector in
// one statement). Returns canceled=true iff a row was updated.
func (c *CancelRepo) CancelPending(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := c.db.ExecContext(ctx,
		`UPDATE receipts SET state = 'canceled', canceled_at = ? WHERE id = ? AND state = 'pending'`,
		rfc3339(at), id)
	if err != nil {
		return false, mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("cancel pending: rows affected: %w", err)
	}
	return n > 0, nil
}

// DeleteTimers removes every timer row tied to receiptID (claimed or not).
// The retry scheduler also short-circuits on the terminal canceled state, so
// this is belt-and-suspenders cleanup that prevents the timer engine from
// re-claiming work for a receipt that will never act on it.
func (c *CancelRepo) DeleteTimers(ctx context.Context, receiptID string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM timers WHERE receipt_id = ?`, receiptID)
	return mapErr(err)
}

// ListCancellableByTag returns the ids of pending receipts carrying tag whose
// parent send belongs to appID, joined through sends so cancel_by_tag touches
// only the calling app's receipts (tenant isolation). Ordered by receipt id
// ascending for stable, observable behavior.
func (c *CancelRepo) ListCancellableByTag(ctx context.Context, tag string, appID int64) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT r.id FROM receipts r
JOIN sends s ON r.send_id = s.id
WHERE r.tag = ? AND r.state = 'pending' AND s.app_id = ?
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
