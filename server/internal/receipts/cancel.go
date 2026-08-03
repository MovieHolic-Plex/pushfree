// This file is the cancel domain logic for todo 24 (cancel + cancel_by_tag),
// written test-first. It owns the rule for when a receipt is cancellable and
// the pure orchestrators that transition a pending receipt (or one tag's worth
// of pending receipts) to the canceled terminal state and clear its scheduled
// timers. It mirrors scheduler.go's RetryStore seam: CancelStore is the narrow
// persistence surface, defined here so this package (and its tests) stay
// decoupled from the concrete SQLite driver. The concrete implementation is
// internal/store/sqlite.CancelRepo; the HTTP surface is internal/api/cancel.go.
package receipts

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Cancellable reports whether a receipt in state s can still be canceled.
//
// Only a pending (queued, not-yet-delivered) receipt is cancellable: cancel
// pulls an emergency alert out of the delivery pipeline before it is handed
// off to a transport. Once a receipt has been delivered, acknowledged,
// expired, or canceled it has progressed past the cancellable point and cancel
// is an error. This matches the cancel endpoint contract (todo 24) and the
// state machine's pending -> canceled transition; the delivered -> canceled
// transition exists in the state machine for completeness but the cancel
// endpoint deliberately does not take it (a delivered alert has already been
// shown and cannot be un-rung).
func Cancellable(s State) bool { return s == StatePending }

// ErrNotCancellable is returned by Cancel when the receipt is not in the
// pending state. It is wrapped with the current state so the API layer can
// surface a meaningful "receipt is <state> and cannot be canceled" message
// (mapped to HTTP 409).
var ErrNotCancellable = errors.New("receipts: receipt is not cancellable")

// notCancellableErr wraps ErrNotCancellable with the offending state, so the
// caller can branch on errors.Is(err, ErrNotCancellable) and still report the
// concrete state in the user-facing message.
func notCancellableErr(s State) error {
	return fmt.Errorf("%w: state is %s", ErrNotCancellable, s)
}

// CancelStore is the narrow persistence surface the cancel operations need
// (todo 24). It is a subset of the store's receipt + timer operations, defined
// here so this package and its tests stay decoupled from the concrete SQLite
// driver -- mirroring the RetryStore seam in scheduler.go. The production
// implementation is internal/store/sqlite.CancelRepo; the in-memory fake in
// cancel_test.go stands in for a real database.
type CancelStore interface {
	// GetReceipt loads the receipt's current lifecycle state and retry_count.
	GetReceipt(ctx context.Context, id string) (ReceiptSnapshot, error)
	// CancelPending atomically transitions a pending receipt to the canceled
	// terminal state, recording canceled_at = at. It returns canceled=true iff
	// the receipt was pending and got transitioned; false if it was already in
	// another state (so the caller can report a meaningful "cannot cancel"
	// error). This conditional UPDATE is the persist half of the cancel
	// endpoint and the race guard: a receipt that leaves pending between a
	// GetReceipt and the UPDATE affects zero rows.
	CancelPending(ctx context.Context, id string, at time.Time) (bool, error)
	// DeleteTimers removes scheduled timers for the receipt. The retry
	// scheduler (todo 21) also short-circuits on the terminal canceled state
	// (it emits EventDone), so this is belt-and-suspenders cleanup that
	// prevents the timer engine (todo 22) from re-claiming dead work.
	DeleteTimers(ctx context.Context, receiptID string) error
	// ListCancellableByTag returns the ids of pending receipts with the given
	// tag whose parent send belongs to appID, so cancel_by_tag touches only the
	// calling app's receipts (tenant isolation: one app cannot cancel another
	// app's tagged alerts). Ordered by id for stable, observable behavior.
	ListCancellableByTag(ctx context.Context, tag string, appID int64) ([]string, error)
}

// Cancel transitions a pending receipt to the canceled terminal state and
// clears its scheduled timers. It returns canceled=true on success.
//
// A non-pending receipt yields (false, ErrNotCancellable) with no mutation;
// the API layer maps that to a 409 "cannot cancel" error. The load-then-
// conditional-update is safe against a concurrent state change: if the receipt
// leaves pending between the Get and the UPDATE, the UPDATE affects zero rows
// and the receipt is re-read so the returned error carries its actual state.
// `at` is the cancel instant (UTC at the API layer for stable storage).
func Cancel(ctx context.Context, s CancelStore, id string, at time.Time) (bool, error) {
	snap, err := s.GetReceipt(ctx, id)
	if err != nil {
		return false, err
	}
	if !Cancellable(snap.State) {
		return false, notCancellableErr(snap.State)
	}
	ok, err := s.CancelPending(ctx, id, at)
	if err != nil {
		return false, err
	}
	if !ok {
		// Raced out of pending between the Get and the conditional UPDATE; the
		// receipt is no longer cancellable. Re-read for an accurate error
		// rather than a generic failure.
		snap, err := s.GetReceipt(ctx, id)
		if err != nil {
			return false, err
		}
		return false, notCancellableErr(snap.State)
	}
	// Timers are belt-and-suspenders: a delete failure must not revert a
	// successful cancel, because the terminal canceled state already makes the
	// scheduler emit EventDone. The error is swallowed; the timer engine will
	// reclaim or GC any stray rows.
	_ = s.DeleteTimers(ctx, id)
	return true, nil
}

// CancelByTag cancels every pending receipt carrying tag and owned by appID,
// returning the ids that were actually transitioned to canceled. It is
// idempotent: receipts already past pending (delivered/acknowledged/expired/
// canceled) are skipped silently. Each receipt is canceled independently
// (CancelPending is the atomic unit); a store error aborts the batch and
// returns the ids canceled so far, so a partial result is observable.
func CancelByTag(ctx context.Context, s CancelStore, tag string, appID int64, at time.Time) ([]string, error) {
	ids, err := s.ListCancellableByTag(ctx, tag, appID)
	if err != nil {
		return nil, err
	}
	canceled := make([]string, 0, len(ids))
	for _, id := range ids {
		ok, err := s.CancelPending(ctx, id, at)
		if err != nil {
			return canceled, err
		}
		if ok {
			_ = s.DeleteTimers(ctx, id)
			canceled = append(canceled, id)
		}
	}
	return canceled, nil
}
