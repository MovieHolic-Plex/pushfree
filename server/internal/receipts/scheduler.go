// This file is the GREEN phase of todo 21: the priority-2 (emergency) retry
// scheduler that makes scheduler_test.go pass. It was written AFTER the test
// was committed RED.
//
// The scheduler is the pure decision+action engine for one receipt's retry/
// expire lifecycle. It owns the policy (30s retry interval, <=3h expire,
// 50-attempt hard cap), the injected clock, and the two side-channels that
// connect a p=2 send to the rest of the system:
//   - RetryStore: the receipt-row persistence surface (retry_count, state,
//     expired_at). This is how p=2 sends connect to receipt ROWS.
//   - Redeliver:  the redelivery push through the hub/FCM/UP channels.
//
// It is deliberately NOT a background loop: the durable driving (polling the
// timers table, atomic ClaimDue, crash recovery, jitter) is owned by the timer
// engine (todo 22). That worker calls Scheduler.Tick per due receipt. Keeping
// the engine pure+clock-driven is what lets the full 0..1500s schedule,
// including the 50-attempt cap, be exercised with zero real sleeps.
package receipts

import (
	"context"
	"time"
)

// Priority-2 (emergency) retry policy constants, matching the Pushover
// emergency-alert contract (plan todo 21 / EB/L1): the retry interval defaults
// to and may not fall below 30 seconds; the expire window defaults to and may
// not exceed 10800 seconds (3 hours); a receipt is forcibly expired after
// MaxAttempts delivery attempts even if the expire window has not elapsed.
const (
	DefaultRetryInterval = 30 * time.Second
	MinRetryInterval     = 30 * time.Second
	DefaultExpire        = 10800 * time.Second // 3 hours
	MaxExpire            = 10800 * time.Second
	MaxAttempts          = 50
)

// RetryPolicy is the per-receipt retry/expire policy. A zero value Normalize()s
// to the Pushover defaults.
type RetryPolicy struct {
	RetryInterval time.Duration
	Expire        time.Duration
	MaxAttempts   int
}

// DefaultRetryPolicy returns the canonical policy: 30s retry interval, 3h
// expire, 50-attempt hard cap.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		RetryInterval: DefaultRetryInterval,
		Expire:        DefaultExpire,
		MaxAttempts:   MaxAttempts,
	}.Normalize()
}

// Normalize applies the defaults and hard limits:
//   - RetryInterval below MinRetryInterval (30s) is raised to 30s;
//   - Expire <= 0 becomes DefaultExpire, above MaxExpire is lowered to MaxExpire;
//   - MaxAttempts <= 0 or above MaxAttempts becomes MaxAttempts.
//
// This is the single place the min/max/default rules are enforced, so callers
// can accept raw user input (the messages.json retry/expire params) and trust
// the result is always in range.
func (p RetryPolicy) Normalize() RetryPolicy {
	if p.RetryInterval < MinRetryInterval {
		p.RetryInterval = MinRetryInterval
	}
	switch {
	case p.Expire <= 0:
		p.Expire = DefaultExpire
	case p.Expire > MaxExpire:
		p.Expire = MaxExpire
	}
	if p.MaxAttempts <= 0 || p.MaxAttempts > MaxAttempts {
		p.MaxAttempts = MaxAttempts
	}
	return p
}

// ExpireReason describes why a receipt was expired by the scheduler.
type ExpireReason string

const (
	// ExpireBecauseCap: the MaxAttempts (50) hard cap was reached before the
	// expire window elapsed.
	ExpireBecauseCap ExpireReason = "cap"
	// ExpireBecauseTimeout: the expire window elapsed before the cap was reached.
	ExpireBecauseTimeout ExpireReason = "timeout"
)

// ExpiredAt reports whether a receipt created at createdAt, with retryCount
// delivery attempts already made, should be expired at now under policy p. The
// cap is checked FIRST, so when both conditions hold the cap reason wins and
// exactly MaxAttempts attempts have run (never MaxAttempts+1). p is normalized.
//
// This is the pure rule behind the hard cap: at expire=60s/interval=30s the
// sequence is attempt1@0, attempt2@+30s, then expired@+60s (2 attempts); at the
// default 3h expire the cap fires at +1500s after exactly 50 attempts.
func ExpiredAt(createdAt, now time.Time, retryCount int, p RetryPolicy) (bool, ExpireReason) {
	p = p.Normalize()
	if retryCount >= p.MaxAttempts {
		return true, ExpireBecauseCap
	}
	if !now.Before(createdAt.Add(p.Expire)) {
		return true, ExpireBecauseTimeout
	}
	return false, ""
}

// NextRetryAt returns the instant the next delivery attempt becomes due:
// createdAt + retryCount*interval. Attempt 1 (retryCount 0) is due immediately
// at createdAt, attempt 2 at createdAt+interval, and so on. p is normalized.
func NextRetryAt(createdAt time.Time, retryCount int, p RetryPolicy) time.Time {
	p = p.Normalize()
	return createdAt.Add(time.Duration(retryCount) * p.RetryInterval)
}

// Clock returns the current instant. Production passes time.Now; tests inject a
// controllable clock so the schedule is exercised without sleeping.
type Clock func() time.Time

// ReceiptSnapshot is the minimal mutable view of a receipt row the Scheduler
// reads each tick: its lifecycle state and how many delivery attempts have run.
type ReceiptSnapshot struct {
	State      State
	RetryCount int
}

// RetryStore is the narrow persistence surface the Scheduler needs against a
// receipt row. It is a subset of the store's receipt operations, defined here
// so the receipts package (and its tests) stay decoupled from the concrete
// SQLite driver. The production adapter wraps store.ReceiptRepo alongside the
// durable timer engine (todo 22); the in-memory implementation in the test
// stands in for a real receipt row.
type RetryStore interface {
	// GetReceipt loads the receipt's current state and retry_count.
	GetReceipt(ctx context.Context, id string) (ReceiptSnapshot, error)
	// IncrementRetry bumps retry_count by one and records `at` as the time of
	// the attempt. It is the persist half of a retry.
	IncrementRetry(ctx context.Context, id string, at time.Time) error
	// SetExpired transitions the receipt to the expired terminal state and
	// records expired_at = at.
	SetExpired(ctx context.Context, id string, at time.Time) error
}

// Redeliver pushes the message again through the hub/FCM/UP channels for one
// retry attempt. attempt is the 1-based attempt number about to be recorded.
//
// API-COMPAT (M6 semantic): WS-only recipients -- those with no FCM/UP token,
// reachable only over the live WS/SSE connection -- receive retries via
// since-cursor replay on reconnect rather than an active push. The retry still
// fires and is counted, but for a WS-only recipient this callback is a
// retention no-op: the message row stays in the recipient's since cursor so the
// next reconnect replays it. This mirrors Pushover, where an unconnected client
// picks up the emergency alert on its next connect. Recording the attempt
// regardless keeps the 50-cap and expire accounting identical across transport
// mixes.
type Redeliver func(ctx context.Context, receiptID string, attempt int) error

// EventKind classifies a Scheduler tick result.
type EventKind int

const (
	// EventNone: nothing was due at the clock's current instant (the next retry
	// time lies in the future).
	EventNone EventKind = iota
	// EventRetry: a redelivery attempt was made (Redeliver called, retry_count
	// bumped).
	EventRetry
	// EventExpire: the receipt was expired -- cap or timeout (Reason carries
	// which).
	EventExpire
	// EventDone: the receipt is already terminal (acknowledged/expired/canceled);
	// no further work remains. Returned idempotently on every subsequent tick.
	EventDone
)

// Event records one Scheduler action. At is the clock instant it occurred at;
// Attempt is the 1-based retry number for EventRetry; Reason is the expire
// cause for EventExpire.
type Event struct {
	Kind    EventKind
	At      time.Time
	Attempt int
	Reason  ExpireReason
}

// Scheduler drives ONE priority-2 receipt's retry/expire lifecycle under an
// injected clock. Construct one per active receipt (at ingest) and call Tick
// whenever the receipt is due.
//
// Tick ordering is load-bearing for the acceptance criteria:
//  1. A terminal receipt yields EventDone (idempotent stop, so ack/cancel/expire
//     observed from outside halts retries immediately).
//  2. The expire check runs BEFORE the retry check, so at the exact expire
//     boundary expire wins and no off-by-one extra attempt fires (this is what
//     makes expire=60s land on exactly 2 attempts).
//  3. The cap check inside ExpiredAt runs before the timeout check, so the
//     50-attempt cap is reported as "cap" even when the expire window has also
//     elapsed.
type Scheduler struct {
	policy    RetryPolicy
	receiptID string
	createdAt time.Time
	clock     Clock
	store     RetryStore
	redeliver Redeliver
}

// NewScheduler builds a Scheduler for one receipt. createdAt is the receipt's
// creation instant (t=0 of its schedule); the first attempt is due immediately
// at createdAt. policy is normalized. A nil clock defaults to time.Now; a nil
// redeliver is a no-op (useful when only the schedule/expire accounting is
// being driven, not the transport).
func NewScheduler(policy RetryPolicy, receiptID string, createdAt time.Time, clock Clock, store RetryStore, redeliver Redeliver) *Scheduler {
	if clock == nil {
		clock = Clock(time.Now)
	}
	if redeliver == nil {
		redeliver = func(context.Context, string, int) error { return nil }
	}
	return &Scheduler{
		policy:    policy.Normalize(),
		receiptID: receiptID,
		createdAt: createdAt,
		clock:     clock,
		store:     store,
		redeliver: redeliver,
	}
}

// Tick processes the next due action at the clock's current instant and returns
// the resulting Event. See the Scheduler doc comment for the ordering contract.
func (s *Scheduler) Tick(ctx context.Context) (Event, error) {
	snap, err := s.store.GetReceipt(ctx, s.receiptID)
	if err != nil {
		return Event{}, err
	}
	if snap.State.Terminal() {
		return Event{Kind: EventDone}, nil
	}
	now := s.clock()
	if exp, reason := ExpiredAt(s.createdAt, now, snap.RetryCount, s.policy); exp {
		if err := s.store.SetExpired(ctx, s.receiptID, now); err != nil {
			return Event{}, err
		}
		return Event{Kind: EventExpire, At: now, Reason: reason}, nil
	}
	due := NextRetryAt(s.createdAt, snap.RetryCount, s.policy)
	if now.Before(due) {
		return Event{Kind: EventNone}, nil
	}
	attempt := snap.RetryCount + 1
	if err := s.redeliver(ctx, s.receiptID, attempt); err != nil {
		return Event{}, err
	}
	if err := s.store.IncrementRetry(ctx, s.receiptID, now); err != nil {
		return Event{}, err
	}
	return Event{Kind: EventRetry, At: now, Attempt: attempt}, nil
}
