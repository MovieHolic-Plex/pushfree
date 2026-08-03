// This file is the todo-22 wiring of the todo-21 priority-2 retry scheduler
// onto the durable timer engine. It is a NEW adapter (not an edit to the
// shared receipts.Scheduler) -- the scheduler stays a pure, single-receipt
// decision engine; this handler is what makes a fired "retry" timer DRIVE one
// scheduler tick and, when the retry continues, schedule the next retry
// timer with ±10% jitter.
//
// The handoff is intentionally one-directional:
//
//   - A priority-2 send (todo 21 ingest) creates the FIRST retry timer with a
//     RetryPayload {receipt_id, created_at} and kind "retry".
//   - When that timer's fire_at falls due, the engine claims it and calls the
//     retry handler registered here.
//   - The handler rebuilds a receipts.Scheduler from the payload + the shared
//     RetryStore/Redeliver, calls Tick exactly once, and:
//   - EventRetry  -> schedule the next retry timer (fire_at = nominal
//     next-retry instant ±10% of one interval), same payload;
//   - EventExpire -> terminal; the receipt is expired, no follow-up;
//   - EventDone   -> terminal (acknowledged/canceled externally); no follow-up.
//
// Because every step is persisted as a timer row before the previous row is
// deleted, a crash between scheduling and firing loses no work: the surviving
// row (old or new) drives the next attempt after Recover.
package timers

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"github.com/pushfree/pushfree/internal/receipts"
	"github.com/pushfree/pushfree/internal/store"
)

// RetryPayload is the JSON carried in a "retry" timer's payload. It carries
// exactly what the handler needs to rebuild a receipts.Scheduler: the receipt
// id and its creation instant (t=0 of its retry schedule). createdAt is
// immutable across the retry chain so the cadence stays anchored to the
// original send, not to whenever the poll happened to run.
type RetryPayload struct {
	ReceiptID string    `json:"receipt_id"`
	CreatedAt time.Time `json:"created_at"`
}

// MarshalPayload serializes p for storage in store.Timer.Payload.
func MarshalPayload(p RetryPayload) (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("timers: marshal retry payload: %w", err)
	}
	return string(b), nil
}

// UnmarshalPayload deserializes a retry timer's payload.
func UnmarshalPayload(s string) (RetryPayload, error) {
	var p RetryPayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return RetryPayload{}, fmt.Errorf("timers: unmarshal retry payload: %w", err)
	}
	return p, nil
}

// NewRetryHandler returns a Handler for kind "retry" that drives one
// receipts.Scheduler.Tick per fire and schedules the next retry timer when
// the receipt is still live.
//
//   - eng        : the engine whose store the next-timer is enqueued against;
//   - rs         : the receipt-row persistence surface (RetryStore);
//   - policy     : the retry/expire/cap policy (normalized inside);
//   - clock      : the injected clock (shared with the engine so due-times
//     agree);
//   - redeliver  : the redelivery push through hub/FCM/UP;
//   - rng        : source of ±10% jitter; nil disables jitter (used in tests
//     that assert the exact nominal cadence).
func NewRetryHandler(
	eng *Engine,
	rs receipts.RetryStore,
	policy receipts.RetryPolicy,
	clock Clock,
	redeliver receipts.Redeliver,
	rng *rand.Rand,
) Handler {
	policy = policy.Normalize()
	return func(ctx context.Context, t store.Timer) error {
		p, err := UnmarshalPayload(t.Payload)
		if err != nil {
			return err
		}
		sched := receipts.NewScheduler(
			policy, p.ReceiptID, p.CreatedAt,
			receipts.Clock(clock), rs, redeliver)
		ev, err := sched.Tick(ctx)
		if err != nil {
			return fmt.Errorf("retry tick %s: %w", p.ReceiptID, err)
		}
		switch ev.Kind {
		case receipts.EventRetry:
			// Schedule the next attempt: nominal = createdAt + attempt*interval
			// (attempt is the 1-based number just recorded, so the NEXT attempt
			// is attempt+1, due at createdAt + attempt*interval). Apply ±10%
			// jitter to ONE interval around the nominal, not the cumulative
			// offset, so the deviation is bounded regardless of attempt count.
			interval := policy.RetryInterval
			nominal := receipts.NextRetryAt(p.CreatedAt, ev.Attempt, policy)
			next := nominal.Add(JitterDuration(interval, rng) - interval)
			payload, err := MarshalPayload(p)
			if err != nil {
				return err
			}
			if _, err := eng.CreateTimer(ctx, &store.Timer{
				Kind: KindRetry, ReceiptID: p.ReceiptID, FireAt: next, Payload: payload,
			}); err != nil {
				return fmt.Errorf("schedule next retry %s: %w", p.ReceiptID, err)
			}
			return nil
		case receipts.EventExpire, receipts.EventDone:
			// Terminal: no follow-up timer. The receipt row already reflects
			// the terminal state (SetExpired ran inside Tick / ack observed).
			return nil
		case receipts.EventNone:
			// Not due yet by the scheduler's clock -- should not happen for a
			// claimed timer, but stay safe: reschedule at the nominal next time
			// so no work is silently dropped.
			snap, gerr := rs.GetReceipt(ctx, p.ReceiptID)
			if gerr != nil {
				return fmt.Errorf("retry recheck %s: %w", p.ReceiptID, gerr)
			}
			next := receipts.NextRetryAt(p.CreatedAt, snap.RetryCount, policy)
			payload, _ := MarshalPayload(p)
			if _, err := eng.CreateTimer(ctx, &store.Timer{
				Kind: KindRetry, ReceiptID: p.ReceiptID, FireAt: next, Payload: payload,
			}); err != nil {
				return err
			}
			return nil
		}
		return nil
	}
}
