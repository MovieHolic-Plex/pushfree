package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// receiptEligibleCutoff is the per-row eligibility predicate for receipt GC.
// A receipt is eligible when it is terminal (acknowledged/expired/canceled)
// AND its terminal timestamp is at or before the cutoff, OR when it is still
// pending/delivered but its expires_at is at or before the cutoff. The cutoff
// instant is bound four times ($1..$4), once per occurrence, matching the
// SQLite predicate's structure (which used strftime comparisons; here the
// native TIMESTAMPTZ columns compare directly with no epoch conversion).
const receiptEligibleCutoff = `(
    (state = 'acknowledged' AND acknowledged_at IS NOT NULL AND acknowledged_at <= $1)
 OR (state = 'expired'      AND expired_at IS NOT NULL      AND expired_at <= $2)
 OR (state = 'canceled'     AND canceled_at IS NOT NULL     AND canceled_at <= $3)
 OR (state IN ('pending', 'delivered') AND expires_at IS NOT NULL AND expires_at <= $4)
)`

// SweepReceipts is the Postgres implementation of store.ReceiptRepo.SweepReceipts
// (todo 23). It garbage-collects receipts whose retention window has elapsed,
// cascading the delete to dependent timers, callbacks and callback-DLQ rows
// so no orphans remain. The FKs are declared ON DELETE NO ACTION (the default),
// so children are deleted explicitly before the receipt parent within one
// transaction. now is injectable so the retention boundary is tested without
// sleeping.
func (r *ReceiptRepo) SweepReceipts(ctx context.Context, now time.Time, retention time.Duration) (store.ReceiptSweepResult, error) {
	cutoff := now.Add(-retention)

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

	// Run the cascade in one transaction. FK order: dlq -> callbacks ->
	// timers -> receipts.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	dlqN, err := execDeleteInTx(ctx, tx, `DELETE FROM dlq WHERE callback_id IN (SELECT id FROM callbacks WHERE receipt_id IN `, ids, 2)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	cbN, err := execDeleteInTx(ctx, tx, `DELETE FROM callbacks WHERE receipt_id IN `, ids, 1)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	tmN, err := execDeleteInTx(ctx, tx, `DELETE FROM timers WHERE receipt_id IN `, ids, 1)
	if err != nil {
		return store.ReceiptSweepResult{}, mapErr(err)
	}
	rcN, err := execDeleteInTx(ctx, tx, `DELETE FROM receipts WHERE id IN `, ids, 1)
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

// execDeleteInTx runs `prefix ($1,$2,...,$N)` with the id list bound in order
// and returns the rows deleted. closeParens closing parens are appended after
// the placeholder list (1 for a flat IN, 2 for a nested IN (... IN (...))).
// The explicit placeholder list keeps this portable across drivers (no array
// binding). q is the transaction (or pool) the cascade runs in.
func execDeleteInTx(ctx context.Context, q queryExec, prefix string, ids []string, closeParens int) (int64, error) {
	place := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		place[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	query := prefix + "(" + strings.Join(place, ",") + strings.Repeat(")", closeParens)
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
