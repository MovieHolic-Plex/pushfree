package sqlite

import (
	"context"
	"time"
)

// This file is the GREEN phase of todo 23 for the ack transition. It is a NEW
// file (not an edit to worker-owned receipt.go): the receipts.send_id 1:1 and
// the receipts schema are owned by todo 5; todo 23 only adds the lifecycle
// writes on top.

// Acknowledge is the SQLite implementation of store.ReceiptRepo.Acknowledge
// (todo 23). See the interface doc for the idempotency / illegal-transition
// contract: a pending/delivered row flips to acknowledged; an already-
// acknowledged row is left untouched (the original metadata is preserved, so
// a re-ack is idempotent); an expired/canceled row is an illegal forward
// transition and a no-op returning nil. An unknown receipt id surfaces as
// store.ErrNotFound so the HTTP endpoint can map it to 404 distinctly from the
// terminal no-op.
//
// Stopping retries is implied by the terminal state: the retry scheduler
// (todo 21) reads the receipt state each tick and emits EventDone once it is
// terminal, so no further retry timer is re-armed. The pending timer rows are
// left to be reclaimed by the timer engine (todo 22) or swept by receipt GC
// (receipt_gc.go) once the retention window elapses.
func (r *ReceiptRepo) Acknowledge(ctx context.Context, id, acknowledgedBy, acknowledgedByDevice string, at time.Time) error {
	// Distinguish "unknown receipt" (ErrNotFound) from "terminal receipt,
	// no-op success": the HTTP ack maps the former to 404, the latter to 200.
	var state string
	if err := r.db.QueryRowContext(ctx, `SELECT state FROM receipts WHERE id = ?`, id).Scan(&state); err != nil {
		return mapErr(err)
	}
	// Conditional UPDATE: only pending/delivered rows change. Already-terminal
	// rows match no row and are therefore untouched, preserving any earlier
	// acknowledged_at / _by / _by_device (idempotent re-ack) or the expired/
	// canceled state (illegal forward transition -> no-op).
	if _, err := r.db.ExecContext(ctx, `UPDATE receipts
SET state = 'acknowledged', acknowledged_at = ?, acknowledged_by = ?, acknowledged_by_device = ?
WHERE id = ? AND state IN ('pending', 'delivered')`,
		rfc3339(at), optStr(acknowledgedBy), optStr(acknowledgedByDevice), id); err != nil {
		return mapErr(err)
	}
	return nil
}
