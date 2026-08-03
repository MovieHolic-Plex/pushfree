package postgres

import (
	"context"
	"time"
)

// Acknowledge is the Postgres implementation of store.ReceiptRepo.Acknowledge
// (todo 23). See the interface doc for the idempotency / illegal-transition
// contract: a pending/delivered row flips to acknowledged; an already-
// acknowledged row is left untouched; an expired/canceled row is an illegal
// forward transition and a no-op returning nil. An unknown receipt id
// surfaces as store.ErrNotFound.
//
// Distinguished from the SQLite ack only by the $N placeholder style; the
// conditional WHERE state IN ('pending','delivered') guard is identical.
func (r *ReceiptRepo) Acknowledge(ctx context.Context, id, acknowledgedBy, acknowledgedByDevice string, at time.Time) error {
	var state string
	if err := r.db.QueryRowContext(ctx, `SELECT state FROM receipts WHERE id = $1`, id).Scan(&state); err != nil {
		return mapErr(err)
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE receipts
SET state = 'acknowledged', acknowledged_at = $1, acknowledged_by = $2, acknowledged_by_device = $3
WHERE id = $4 AND state IN ('pending', 'delivered')`,
		at, optStr(acknowledgedBy), optStr(acknowledgedByDevice), id); err != nil {
		return mapErr(err)
	}
	return nil
}
