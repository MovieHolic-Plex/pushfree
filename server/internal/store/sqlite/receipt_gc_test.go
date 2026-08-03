package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// seedReceiptAt inserts one send + one crafted receipt (state + terminal /
// expiry timestamps fully controlled by the caller) and returns the receipt
// id. seed picks the deterministic 30-char receipt id (unique per call). Used
// to build rows at known ages so the GC boundary is exercised with an
// injected clock rather than sleeping.
func seedReceiptAt(t *testing.T, s *Store, appID, senderID int64, seed int, r *store.Receipt) string {
	t.Helper()
	r.ID = key30(seed)
	sendID, err := s.sends.CreateFanout(context.Background(), &store.Fanout{
		Send: store.Send{
			AppID:        appID,
			SenderUserID: senderID,
			Priority:     2,
			Body:         "gc",
			CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		Messages: []store.Message{{RecipientUserID: senderID, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}},
		Receipt:  r,
	})
	if err != nil {
		t.Fatalf("seedReceiptAt CreateFanout: %v", err)
	}
	r.SendID = sendID
	return r.ID
}

// TestReceiptGCRetentionBoundary verifies the 7-day receipt retention sweep at
// the store level, asserting against the raw table state (the
// misleading_success_output guard: we do not trust the result struct alone).
//
//	acknowledged + >7d    -> swept
//	acknowledged + fresh  -> kept
//	pending + expires past-> swept (unacked past expiry window)
//	pending + expires future-> kept
//
// and that the cascade removes the receipt's timers, callbacks and dlq rows
// (FK order: children before the receipt parent).
func TestReceiptGCRetentionBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	owner := mustSeedUser(t, s.users, 801, "gc@example.com")
	app := mustSeedApp(t, s.apps, owner.ID, 802)

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const retention = 7 * 24 * time.Hour
	old := now.Add(-8 * 24 * time.Hour)   // >7d: eligible
	fresh := now.Add(-6 * 24 * time.Hour) // <7d: kept
	pastExp := now.Add(-10 * 24 * time.Hour)
	futExp := now.Add(1 * time.Hour)

	oldID := seedReceiptAt(t, s, app.ID, owner.ID, 810, &store.Receipt{
		State: "acknowledged", AcknowledgedAt: &old,
	})
	seedReceiptAt(t, s, app.ID, owner.ID, 811, &store.Receipt{
		State: "acknowledged", AcknowledgedAt: &fresh,
	})
	seedReceiptAt(t, s, app.ID, owner.ID, 812, &store.Receipt{
		State: "pending", ExpiresAt: &pastExp,
	})
	seedReceiptAt(t, s, app.ID, owner.ID, 813, &store.Receipt{
		State: "pending", ExpiresAt: &futExp,
	})

	// Dependents on the old (doomed) receipt: a timer, a callback, and a dlq
	// row hanging off the callback. All must vanish with the receipt.
	timerID, err := s.timrs.Create(ctx, &store.Timer{
		Kind: "retry", ReceiptID: oldID, FireAt: old, Payload: "{}",
	})
	if err != nil {
		t.Fatalf("seed timer: %v", err)
	}
	cbID, err := s.cbs.Create(ctx, &store.Callback{
		ReceiptID: oldID, URL: "https://example.com/cb", State: "pending",
	})
	if err != nil {
		t.Fatalf("seed callback: %v", err)
	}
	if _, err := s.cbs.CreateDLQ(ctx, &store.DLQ{
		CallbackID: cbID, LastError: "boom", At: old, Attempts: 3,
	}); err != nil {
		t.Fatalf("seed dlq: %v", err)
	}

	if got := countRows(t, s, `SELECT count(*) FROM receipts`); got != 4 {
		t.Fatalf("receipts before sweep = %d, want 4", got)
	}

	res, err := s.rcpts.SweepReceipts(ctx, now, retention)
	if err != nil {
		t.Fatalf("SweepReceipts: %v", err)
	}
	if res.Receipts != 2 {
		t.Fatalf("swept receipts = %d, want 2 (old acked + unacked-past-expiry)", res.Receipts)
	}
	if res.Timers != 1 || res.Callbacks != 1 || res.DLQ != 1 {
		t.Fatalf("cascade counts = timers:%d callbacks:%d dlq:%d, want 1/1/1", res.Timers, res.Callbacks, res.DLQ)
	}

	// Raw table state after sweep: 2 receipts remain, all dependents gone.
	if got := countRows(t, s, `SELECT count(*) FROM receipts`); got != 2 {
		t.Fatalf("receipts after sweep = %d, want 2", got)
	}
	if got := countRows(t, s, `SELECT count(*) FROM timers WHERE id = ?`, timerID); got != 0 {
		t.Fatalf("timer for swept receipt still present: %d", got)
	}
	if got := countRows(t, s, `SELECT count(*) FROM callbacks WHERE id = ?`, cbID); got != 0 {
		t.Fatalf("callback for swept receipt still present: %d", got)
	}
	if got := countRows(t, s, `SELECT count(*) FROM dlq WHERE callback_id = ?`, cbID); got != 0 {
		t.Fatalf("dlq for swept receipt still present: %d", got)
	}

	// The old receipt itself is gone; the fresh acknowledged one survives.
	if _, err := s.rcpts.GetByID(ctx, oldID); err == nil {
		t.Fatalf("old receipt still retrievable after sweep")
	}
}

// TestReceiptGCAcknowledgeTransition verifies Acknowledge flips a pending
// receipt to acknowledged, is idempotent (second call preserves the original
// acknowledged_at), and is a no-op on an expired receipt (illegal forward
// transition returns nil without changing state).
func TestReceiptGCAcknowledgeTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	owner := mustSeedUser(t, s.users, 811, "ack@example.com")
	app := mustSeedApp(t, s.apps, owner.ID, 812)

	pendingID := seedReceiptAt(t, s, app.ID, owner.ID, 820, &store.Receipt{State: "pending"})
	deliveredID := seedReceiptAt(t, s, app.ID, owner.ID, 821, &store.Receipt{State: "delivered"})
	expiredID := seedReceiptAt(t, s, app.ID, owner.ID, 822, &store.Receipt{State: "expired"})

	first := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)

	// pending -> acknowledged, recording by/by_device.
	if err := s.rcpts.Acknowledge(ctx, pendingID, "userkeyA", "phone", first); err != nil {
		t.Fatalf("acknowledge pending: %v", err)
	}
	rc, err := s.rcpts.GetByID(ctx, pendingID)
	if err != nil {
		t.Fatalf("get pending: %v", err)
	}
	if rc.State != "acknowledged" {
		t.Fatalf("state=%q want acknowledged", rc.State)
	}
	if rc.AcknowledgedBy != "userkeyA" || rc.AcknowledgedByDevice != "phone" {
		t.Fatalf("ack by=%q/%q want userkeyA/phone", rc.AcknowledgedBy, rc.AcknowledgedByDevice)
	}
	if rc.AcknowledgedAt == nil || !rc.AcknowledgedAt.Equal(first) {
		t.Fatalf("acknowledged_at=%v want %v", rc.AcknowledgedAt, first)
	}

	// delivered -> acknowledged.
	if err := s.rcpts.Acknowledge(ctx, deliveredID, "userkeyB", "laptop", first); err != nil {
		t.Fatalf("acknowledge delivered: %v", err)
	}
	if rc, _ := s.rcpts.GetByID(ctx, deliveredID); rc.State != "acknowledged" {
		t.Fatalf("delivered state=%q want acknowledged", rc.State)
	}

	// Idempotent: re-ack does NOT overwrite the original at/by.
	if err := s.rcpts.Acknowledge(ctx, pendingID, "userkeyC", "watch", second); err != nil {
		t.Fatalf("idempotent acknowledge: %v", err)
	}
	rc, _ = s.rcpts.GetByID(ctx, pendingID)
	if !rc.AcknowledgedAt.Equal(first) || rc.AcknowledgedBy != "userkeyA" || rc.AcknowledgedByDevice != "phone" {
		t.Fatalf("idempotent re-ack clobbered original: at=%v by=%q/%q", rc.AcknowledgedAt, rc.AcknowledgedBy, rc.AcknowledgedByDevice)
	}

	// Expired is terminal: ack is a no-op (nil, no state change).
	if err := s.rcpts.Acknowledge(ctx, expiredID, "userkeyD", "x", first); err != nil {
		t.Fatalf("acknowledge expired should be no-op nil, got %v", err)
	}
	if rc, _ := s.rcpts.GetByID(ctx, expiredID); rc.State != "expired" {
		t.Fatalf("expired state changed to %q", rc.State)
	}

	// Unknown receipt id: not found, surfaces an error (no silent success).
	if err := s.rcpts.Acknowledge(ctx, key30(999), "x", "y", first); err == nil {
		t.Fatalf("acknowledge unknown id returned nil; want not-found error")
	}
}
