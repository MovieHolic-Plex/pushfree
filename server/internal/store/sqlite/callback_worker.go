package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pushfree/pushfree/internal/callbacks"
	"github.com/pushfree/pushfree/internal/store"
)

// This file is the SQLite implementation of callbacks.Store (todo 25). It is a
// NEW file, deliberately NOT an edit to the worker-owned callback.go: the
// callback worker's persistence surface is isolated here so it composes with
// concurrent work on the shared CallbackRepo (todo 26 chaos) without touching
// that file. It reuses the package-local helpers (rfc3339, nullTime, nullStr,
// mapErr, receiptCols) but owns no shared state.
//
// Layering (mirrors sqlite/cancel.go): the driver (this package) implements a
// domain interface (callbacks.Store) -- dependency inversion, the concrete
// driver depends on the domain abstraction, not the reverse -- which keeps the
// callback surface out of the shared store.go interface file that concurrent
// workers may be editing, so this file commits cleanly on its own.

// CallbackWorkerRepo is the SQLite implementation of callbacks.Store. It wraps
// a single *sql.DB (the same pool the rest of the store uses) and is
// constructed by NewCallbackWorkerRepo at server-wiring time.
type CallbackWorkerRepo struct{ db *sql.DB }

// NewCallbackWorkerRepo builds a CallbackWorkerRepo over an already-open
// *sql.DB. The caller owns the connection lifecycle (the *sqlite.Store
// outlives the repo). Exported so server wiring (cmd/pushfree) and tests can
// construct it without depending on the unexported DB interface.
func NewCallbackWorkerRepo(db *sql.DB) *CallbackWorkerRepo { return &CallbackWorkerRepo{db: db} }

// GetTarget loads a receipt joined to its parent send's callback_url in one
// read. An unknown receipt id surfaces as store.ErrNotFound so the worker can
// skip it. The receipt's 13 columns and the send's callback_url are scanned in
// a single pass (a *sql.Row can only be Scanned once).
func (c *CallbackWorkerRepo) GetTarget(ctx context.Context, receiptID string) (callbacks.Target, error) {
	row := c.db.QueryRowContext(ctx, `
SELECT r.id, r.send_id, r.state, r.tag, r.retry_count, r.expires_at, r.acknowledged_at,
	r.acknowledged_by, r.acknowledged_by_device, r.last_delivered_at, r.called_back_at,
	r.expired_at, r.canceled_at, s.callback_url
FROM receipts r JOIN sends s ON r.send_id = s.id
WHERE r.id = ?`, receiptID)
	var (
		rc  store.Receipt
		tag sql.NullString
		expiresAt,
		ackAt,
		lastDeliv,
		calledBack,
		expiredAt,
		canceledAt sql.NullString
		ackBy,
		ackByDev sql.NullString
		cbURL sql.NullString
	)
	if err := row.Scan(&rc.ID, &rc.SendID, &rc.State, &tag, &rc.RetryCount, &expiresAt, &ackAt,
		&ackBy, &ackByDev, &lastDeliv, &calledBack, &expiredAt, &canceledAt, &cbURL); err != nil {
		return callbacks.Target{}, mapErr(err)
	}
	rc.Tag = nullStr(tag)
	rc.ExpiresAt = nullTime(expiresAt)
	rc.AcknowledgedAt = nullTime(ackAt)
	rc.AcknowledgedBy = nullStr(ackBy)
	rc.AcknowledgedByDevice = nullStr(ackByDev)
	rc.LastDeliveredAt = nullTime(lastDeliv)
	rc.CalledBackAt = nullTime(calledBack)
	rc.ExpiredAt = nullTime(expiredAt)
	rc.CanceledAt = nullTime(canceledAt)
	return callbacks.Target{Receipt: rc, CallbackURL: nullStr(cbURL)}, nil
}

// CreateCallback inserts a pending callback row due at now (attempts=0) and
// returns its id. next_attempt_at=now makes it immediately eligible for the
// next ProcessDue claim.
func (c *CallbackWorkerRepo) CreateCallback(ctx context.Context, receiptID, u string, now time.Time) (int64, error) {
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO callbacks(receipt_id, url, state, next_attempt_at, attempts)
		 VALUES (?, ?, 'pending', ?, 0)`,
		receiptID, u, rfc3339(now))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("callback create: last insert id: %w", err)
	}
	return id, nil
}

// ListDue returns up to limit due callbacks: not done and due at or before
// now. Rows with NULL next_attempt_at (e.g. hand-inserted) are treated as due
// immediately and ordered first.
func (c *CallbackWorkerRepo) ListDue(ctx context.Context, now time.Time, limit int) ([]store.Callback, error) {
	rows, err := c.db.QueryContext(ctx, `
SELECT id, receipt_id, url, state, next_attempt_at, attempts FROM callbacks
WHERE state <> 'done' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
ORDER BY (next_attempt_at IS NULL) DESC, next_attempt_at, id
LIMIT ?`, rfc3339(now), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Callback
	for rows.Next() {
		var (
			cb   store.Callback
			next sql.NullString
		)
		if err := rows.Scan(&cb.ID, &cb.ReceiptID, &cb.URL, &cb.State, &next, &cb.Attempts); err != nil {
			return nil, mapErr(err)
		}
		cb.NextAttemptAt = nullTime(next)
		out = append(out, cb)
	}
	return out, mapErr(rows.Err())
}

// MarkFailed records a failed attempt: state=failed, attempts=attempts,
// next_attempt_at=next.
func (c *CallbackWorkerRepo) MarkFailed(ctx context.Context, id int64, next time.Time, attempts int) error {
	if _, err := c.db.ExecContext(ctx,
		`UPDATE callbacks SET state='failed', next_attempt_at=?, attempts=? WHERE id=?`,
		rfc3339(next), attempts, id); err != nil {
		return mapErr(err)
	}
	return nil
}

// MarkDone marks the callback done and records receipt.called_back_at in one
// transaction. Setting called_back_at is the observable signal that the
// webhook was delivered (2xx); it is exactly the field GET
// /1/receipts/{receipt}.json reports as called_back/_at.
func (c *CallbackWorkerRepo) MarkDone(ctx context.Context, id int64, receiptID string, at time.Time) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("callback mark done: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE callbacks SET state='done', next_attempt_at=NULL WHERE id=?`, id); err != nil {
		return mapErr(err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE receipts SET called_back_at=? WHERE id=?`, rfc3339(at), receiptID); err != nil {
		return mapErr(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("callback mark done: commit: %w", err)
	}
	return nil
}

// RecordDLQ appends a dead-letter row for observability. It never aborts
// retry; the caller schedules the next attempt independently.
func (c *CallbackWorkerRepo) RecordDLQ(ctx context.Context, callbackID int64, lastErr string, at time.Time, attempts int) error {
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO dlq(callback_id, last_error, at, attempts) VALUES (?, ?, ?, ?)`,
		callbackID, lastErr, rfc3339(at), attempts)
	return mapErr(err)
}
