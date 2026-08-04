// This file is owned by todo 22 (durable timer engine). It extends the
// timer storage surface with the two operations the engine needs that the
// todo-5 TimerRepo interface deliberately does not expose (to avoid
// widening the shared store.TimerRepo contract): per-row Delete
// (fire-then-delete) and ResetOrphanedClaims (crash-recovery startup scan).
//
// These are NEW methods on the existing *TimerRepo struct, added in a
// separate file so timer.go (todo 5) is not edited and workers 23/24
// (receipts endpoints/GC and cancel) cannot conflict with it. *TimerRepo
// satisfies the timers.Store interface structurally; no compile-time
// assertion is placed here so internal/store/sqlite never imports
// internal/timers (which would invert the layering).
package sqlite

import (
	"context"
	"fmt"
)

// Delete removes one timer row by id. The engine calls it after a timer has
// fired (fire-then-delete), so a surviving timer row is always work still
// pending -- never completed work. It is not an error if the id is absent
// (idempotent delete), which makes a retry after a crash between fire and
// delete safe.
func (t *TimerRepo) Delete(ctx context.Context, id int64) error {
	if _, err := t.db.ExecContext(ctx, `DELETE FROM timers WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete timer %d: %w", id, mapErr(err))
	}
	return nil
}

// ResetOrphanedClaims clears claimed_at on every currently-claimed timer,
// returning the count of rows reset. The engine calls this exactly once at
// startup, BEFORE the due-poll begins: any timer left with a non-null
// claimed_at belonged to a worker that died between ClaimDue and Delete
// (kill -9). Resetting returns those rows to the unclaimed due-set so the
// poll reclaims and fires them exactly once.
//
// This is the single statement that makes startup recovery race-free: it is
// one UPDATE under the SQLite write lock, not a scan-then-update.
func (t *TimerRepo) ResetOrphanedClaims(ctx context.Context) (int, error) {
	res, err := t.db.ExecContext(ctx, `UPDATE timers SET claimed_at = NULL WHERE claimed_at IS NOT NULL`)
	if err != nil {
		return 0, fmt.Errorf("reset orphaned timer claims: %w", mapErr(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reset orphaned timer claims rows-affected: %w", err)
	}
	return int(n), nil
}

// TimerEngine returns the concrete *TimerRepo so the timers engine can be
// wired against the methods added in this file (Delete, ResetOrphanedClaims)
// in addition to Create/ClaimDue. Exposing the concrete struct (rather than
// widening store.TimerRepo) keeps the shared interface stable for todos 5,
// 23, and 24 while letting todo 22 reach its full storage surface.
func (s *Store) TimerEngine() *TimerRepo { return s.timrs }

// ReceiptRepo returns the concrete *ReceiptRepo so the timer engine's retry
// handler can be wired against the receipts.RetryStore surface
// (GetReceipt/IncrementRetry/SetExpired) in addition to store.ReceiptRepo.
func (s *Store) ReceiptRepo() *ReceiptRepo { return s.rcpts }
