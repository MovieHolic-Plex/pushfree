package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// This file is the GREEN phase of todo 23 for the receipt retention GC. It is
// a NEW file (the receipts schema is owned by todo 5; this only adds a
// lifecycle sweep on top). Time comparisons use strftime('%s', col) (epoch
// seconds) exactly like retention.go, so the result is independent of any
// sub-second formatting differences in the stored TEXT.

// receiptEligibleCutoff is the per-row eligibility predicate for GC. A receipt
// is eligible when it is terminal (acknowledged/expired/canceled) AND its
// terminal timestamp is at or before the cutoff, OR when it is still pending/
// delivered but its expires_at is at or before the cutoff (an unacked
// emergency whose scheduler-driven expiry has not been recorded). ? is the
// cutoff instant, bound once per occurrence.
const receiptEligibleCutoff = `(
    (state = 'acknowledged' AND acknowledged_at IS NOT NULL
        AND CAST(strftime('%s', acknowledged_at) AS INTEGER) <= CAST(strftime('%s', ?) AS INTEGER))
 OR (state = 'expired' AND expired_at IS NOT NULL
        AND CAST(strftime('%s', expired_at) AS INTEGER) <= CAST(strftime('%s', ?) AS INTEGER))
 OR (state = 'canceled' AND canceled_at IS NOT NULL
        AND CAST(strftime('%s', canceled_at) AS INTEGER) <= CAST(strftime('%s', ?) AS INTEGER))
 OR (state IN ('pending', 'delivered') AND expires_at IS NOT NULL
        AND CAST(strftime('%s', expires_at) AS INTEGER) <= CAST(strftime('%s', ?) AS INTEGER))
)`

// SweepReceipts is the SQLite implementation of store.ReceiptRepo.SweepReceipts
// (todo 23). It garbage-collects receipts whose 7-day (configurable) query
// window has elapsed, cascading the delete to the dependent timers, callbacks
// and callback-DLQ rows so no orphans remain (foreign_keys=1 enforces the
// order: children before the receipt parent). now is injectable so the
// retention boundary is tested without sleeping.
//
// The eligible id set is computed once and the deletes are run in a single
// transaction, so a concurrent writer cannot interleave a partial cleanup.
func (r *ReceiptRepo) SweepReceipts(ctx context.Context, now time.Time, retention time.Duration) (store.ReceiptSweepResult, error) {
	cutoff := rfc3339(now.Add(-retention))

	rows, err := r.db.QueryContext(ctx,
		`SELECT id FROM receipts WHERE `+receiptEligibleCutoff,
		cutoff, cutoff, cutoff, cutoff)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return store.ReceiptSweepResult{}, mapErr(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	rows.Close()

	if len(ids) == 0 {
		return store.ReceiptSweepResult{}, nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	// On any failure roll back so a partial cascade never leaves orphans.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// FK order: dlq -> callbacks -> timers -> receipts. Each DELETE is scoped
	// to exactly the eligible receipt set via the precomputed id list. The
	// dlq statement carries a nested IN (callback_id IN (SELECT ... WHERE
	// receipt_id IN (...))) and so needs two closing parens; the others need
	// one. execDeleteIn appends that many closers after the placeholder list.
	dlqN, err := execDeleteIn(ctx, tx, `DELETE FROM dlq WHERE callback_id IN (SELECT id FROM callbacks WHERE receipt_id IN `, ids, 2)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	cbN, err := execDeleteIn(ctx, tx, `DELETE FROM callbacks WHERE receipt_id IN `, ids, 1)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	tmN, err := execDeleteIn(ctx, tx, `DELETE FROM timers WHERE receipt_id IN `, ids, 1)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	rcN, err := execDeleteIn(ctx, tx, `DELETE FROM receipts WHERE id IN `, ids, 1)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}

	if err := tx.Commit(); err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	committed = true
	return store.ReceiptSweepResult{
		Receipts:  rcN,
		Timers:    tmN,
		Callbacks: cbN,
		DLQ:       dlqN,
	}, nil
}

// execDeleteIn runs `prefix (?,...,?)` with the given id list as args and
// returns the number of rows deleted. prefix is the literal SQL up to and
// including the trailing "IN " keyword; closeParens closing parens are
// appended after the placeholder list (1 for a flat IN, 2 for a nested
// IN (... IN (...))).
func execDeleteIn(ctx context.Context, q queryExec, prefix string, ids []string, closeParens int) (int64, error) {
	query := prefix + "(" + strings.Repeat("?,", len(ids)-1) + "?" + strings.Repeat(")", closeParens)
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}
