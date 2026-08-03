// Package receipts is implemented test-first. This file is the RED phase of
// todo 21 (priority-2 emergency retry scheduler). It is committed BEFORE
// scheduler.go exists, so `go test ./internal/receipts/...` fails to build
// (undefined Scheduler/policy symbols) until the implementation lands -- the
// failing-first discipline is auditable in the evidence file.
//
// Every case drives an INJECTED clock (a settable fakeClock): tests set the
// current instant and call Tick. There are zero real sleeps, polling delays,
// or wait-for-time patterns -- the acceptance gate forbids them and the retry
// schedule is fully deterministic.
package receipts

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a controllable Clock. Tests set its current instant directly
// and then call Tick, so the entire 0..1500s retry schedule runs in microseconds
// with no time.Sleep.
type fakeClock struct{ now time.Time }

func (f *fakeClock) clock() Clock { return func() time.Time { return f.now } }

// memStore is an in-memory RetryStore. It tracks retry_count and the terminal
// state so the Scheduler's full read/increment/expire path against a receipt
// row is exercised without a database. It stands in for store.ReceiptRepo at
// the receipts layer (the production adapter is wired alongside todo 22).
type memStore struct {
	mu         sync.Mutex
	state      State
	retryCount int
	expiredAt  *time.Time
}

func newMemStore() *memStore { return &memStore{state: StatePending} }

func (m *memStore) GetReceipt(context.Context, string) (ReceiptSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ReceiptSnapshot{State: m.state, RetryCount: m.retryCount}, nil
}

func (m *memStore) IncrementRetry(_ context.Context, _ string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryCount++
	_ = at
	return nil
}

func (m *memStore) SetExpired(_ context.Context, _ string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = StateExpired
	m.expiredAt = &at
	return nil
}

// redeliverRecorder captures every redelivery attempt the Scheduler triggers,
// in order, so tests assert the exact raw sequence (the manual-QA evidence).
type redeliverRecorder struct {
	mu       sync.Mutex
	attempts []int
}

func (r *redeliverRecorder) fn() Redeliver {
	return func(_ context.Context, _ string, attempt int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.attempts = append(r.attempts, attempt)
		return nil
	}
}

func (r *redeliverRecorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.attempts))
	copy(out, r.attempts)
	return out
}

// driveTicks advances the fake clock to base+off for each offset and calls
// Tick once, collecting every non-EventNone result. It fatals on any Tick
// error so a transport/store failure is not silently swallowed.
func driveTicks(t *testing.T, s *Scheduler, fc *fakeClock, base time.Time, offsets []time.Duration) []Event {
	t.Helper()
	ctx := context.Background()
	var events []Event
	for _, off := range offsets {
		fc.now = base.Add(off)
		ev, err := s.Tick(ctx)
		if err != nil {
			t.Fatalf("Tick at +%v: unexpected error %v", off, err)
		}
		if ev.Kind != EventNone {
			events = append(events, ev)
		}
	}
	return events
}

// TestRetryScheduler is the todo-21 acceptance entry point. It verifies, under
// an injected clock with NO real sleeps:
//   - the retry interval is exactly 30s (attempts at 0,30,60,90,120s);
//   - the 50-attempt HARD CAP: exactly 50 delivery attempts, then expired with
//     reason "cap", and NO 51st attempt, even though the default 3h expire
//     window has not elapsed;
//   - expire=60s -> exactly 2 attempts then expired with reason "timeout";
//   - policy normalization (defaults + min/max clamping);
//   - ExpiredAt precedence (cap beats timeout) and the NextRetryAt cadence.
func TestRetryScheduler(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

	// Exact30sInterval: attempts fire at exactly 0,30,60,90,120s. A 5-attempt
	// cap with a long expire isolates the cadence from the expire window.
	t.Run("Exact30sInterval", func(t *testing.T) {
		fc := &fakeClock{now: base}
		store := newMemStore()
		rec := &redeliverRecorder{}
		s := NewScheduler(
			RetryPolicy{RetryInterval: 30 * time.Second, Expire: time.Hour, MaxAttempts: 5},
			"r1", base, fc.clock(), store, rec.fn())

		events := driveTicks(t, s, fc, base, []time.Duration{
			0, 30 * time.Second, 60 * time.Second, 90 * time.Second,
			120 * time.Second, 150 * time.Second,
		})

		// Five retries at the exact 30s cadence, then a cap-expire at +150s.
		var retryAts []time.Duration
		for _, ev := range events {
			if ev.Kind == EventRetry {
				retryAts = append(retryAts, ev.At.Sub(base))
			}
		}
		want := []time.Duration{0, 30 * time.Second, 60 * time.Second, 90 * time.Second, 120 * time.Second}
		if !equalDurations(retryAts, want) {
			t.Fatalf("retry cadence = %v, want exact 30s multiples %v", retryAts, want)
		}
		if got := rec.snapshot(); len(got) != 5 {
			t.Fatalf("redeliver attempts = %v, want 5", got)
		}
		// The 6th tick (at +150s) must cap-expire, not retry a 6th time.
		last := events[len(events)-1]
		if last.Kind != EventExpire || last.Reason != ExpireBecauseCap {
			t.Fatalf("last event = %+v, want cap-expire", last)
		}
	})

	// FiftyAttemptHardCap: default policy (expire=10800s/3h, interval=30s). The
	// cap fires after exactly 50 attempts; the +1500s tick expires with reason
	// "cap". The 3h expire window has NOT elapsed (1500s << 10800s), proving
	// the hard cap is independent of the expire timeout. No 51st attempt.
	t.Run("FiftyAttemptHardCap", func(t *testing.T) {
		fc := &fakeClock{now: base}
		store := newMemStore()
		rec := &redeliverRecorder{}
		s := NewScheduler(DefaultRetryPolicy(), "r50", base, fc.clock(), store, rec.fn())

		offsets := make([]time.Duration, 0, 51)
		for i := 0; i <= 50; i++ { // 0,30,...,1500 (51 ticks)
			offsets = append(offsets, time.Duration(i)*30*time.Second)
		}
		events := driveTicks(t, s, fc, base, offsets)

		retries, expires := countKinds(events)
		if retries != 50 {
			t.Fatalf("retry events = %d, want exactly 50 (hard cap)", retries)
		}
		if expires != 1 {
			t.Fatalf("expire events = %d, want 1", expires)
		}
		// The single expire is the cap, NOT the 3h timeout (only ~25min elapsed).
		expEv := findExpire(events)
		if expEv.Reason != ExpireBecauseCap {
			t.Fatalf("expire reason = %q, want %q (cap must fire before 3h timeout)",
				expEv.Reason, ExpireBecauseCap)
		}
		// No 51st attempt: the recorder saw attempts 1..50 only.
		got := rec.snapshot()
		if len(got) != 50 || got[0] != 1 || got[49] != 50 {
			t.Fatalf("attempts = %v, want exactly [1..50] (no 51st)", got)
		}
	})

	// ExpireTimeoutTwoAttempts: expire=60s (interval=30s). Two attempts fire at
	// +0s and +30s; at +60s the expire window has elapsed so the receipt is
	// expired with reason "timeout" and NO third attempt is made. This is the
	// failure-case manual-QA scenario, raw.
	t.Run("ExpireTimeoutTwoAttempts", func(t *testing.T) {
		fc := &fakeClock{now: base}
		store := newMemStore()
		rec := &redeliverRecorder{}
		s := NewScheduler(
			RetryPolicy{Expire: 60 * time.Second},
			"r60", base, fc.clock(), store, rec.fn())

		events := driveTicks(t, s, fc, base, []time.Duration{
			0, 30 * time.Second, 60 * time.Second,
		})

		retries, expires := countKinds(events)
		if retries != 2 || expires != 1 {
			t.Fatalf("retries=%d expires=%d, want 2 retries then 1 expire", retries, expires)
		}
		expEv := findExpire(events)
		if expEv.Reason != ExpireBecauseTimeout {
			t.Fatalf("expire reason = %q, want %q", expEv.Reason, ExpireBecauseTimeout)
		}
		got := rec.snapshot()
		if len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Fatalf("attempts = %v, want [1 2]", got)
		}
		// The store is now terminal: a further tick is an idempotent no-op.
		fc.now = base.Add(120 * time.Second)
		ev, err := s.Tick(context.Background())
		if err != nil {
			t.Fatalf("post-expire Tick: %v", err)
		}
		if ev.Kind != EventDone {
			t.Fatalf("post-expire Tick = %+v, want EventDone (no further work)", ev)
		}
	})

	// PolicyNormalization: zero/undersized/oversized values collapse to the
	// Pushover defaults and hard limits (retry>=30s, expire<=10800s, cap=50).
	t.Run("PolicyNormalization", func(t *testing.T) {
		zero := RetryPolicy{}.Normalize()
		if zero.RetryInterval != DefaultRetryInterval {
			t.Errorf("zero RetryInterval = %v, want default %v", zero.RetryInterval, DefaultRetryInterval)
		}
		if zero.Expire != DefaultExpire {
			t.Errorf("zero Expire = %v, want default %v", zero.Expire, DefaultExpire)
		}
		if zero.MaxAttempts != MaxAttempts {
			t.Errorf("zero MaxAttempts = %d, want %d", zero.MaxAttempts, MaxAttempts)
		}
		clamped := RetryPolicy{
			RetryInterval: 5 * time.Second, Expire: 99999 * time.Second, MaxAttempts: 999,
		}.Normalize()
		if clamped.RetryInterval != MinRetryInterval {
			t.Errorf("RetryInterval = %v, want min %v", clamped.RetryInterval, MinRetryInterval)
		}
		if clamped.Expire != MaxExpire {
			t.Errorf("Expire = %v, want max %v", clamped.Expire, MaxExpire)
		}
		if clamped.MaxAttempts != MaxAttempts {
			t.Errorf("MaxAttempts = %d, want %d", clamped.MaxAttempts, MaxAttempts)
		}
	})

	// ExpiredAtPrecedence: when BOTH the cap and the timeout are satisfied, the
	// cap reason wins (it is checked first), so the boundary receipt is reported
	// cap-expired, not timeout-expired.
	t.Run("ExpiredAtPrecedence", func(t *testing.T) {
		p := RetryPolicy{RetryInterval: 30 * time.Second, Expire: 60 * time.Second, MaxAttempts: 2}
		// retryCount==2 (cap) and now past the 60s expire: cap wins.
		exp, reason := ExpiredAt(base, base.Add(70*time.Second), 2, p)
		if !exp || reason != ExpireBecauseCap {
			t.Fatalf("ExpiredAt(cap+timeout) = (%v,%q), want (true,cap)", exp, reason)
		}
		// retryCount==1, past expire: timeout.
		exp, reason = ExpiredAt(base, base.Add(70*time.Second), 1, p)
		if !exp || reason != ExpireBecauseTimeout {
			t.Fatalf("ExpiredAt(timeout only) = (%v,%q), want (true,timeout)", exp, reason)
		}
		// retryCount==1, within expire: not expired.
		exp, reason = ExpiredAt(base, base.Add(30*time.Second), 1, p)
		if exp {
			t.Fatalf("ExpiredAt(within window) = (true,%q), want not expired", reason)
		}
	})

	// NextRetryAt cadence: attempt k is due at createdAt + (k-1)*interval, so
	// attempt 1 is immediate and the spacing is exactly the interval.
	t.Run("NextRetryAtCadence", func(t *testing.T) {
		p := RetryPolicy{RetryInterval: 30 * time.Second, Expire: time.Hour, MaxAttempts: 50}
		cases := []struct {
			retryCount int
			wantOff    time.Duration
		}{
			{0, 0},                      // attempt 1 immediately
			{1, 30 * time.Second},       // attempt 2
			{2, 60 * time.Second},       // attempt 3
			{49, 49 * 30 * time.Second}, // attempt 50
		}
		for _, c := range cases {
			got := NextRetryAt(base, c.retryCount, p).Sub(base)
			if got != c.wantOff {
				t.Errorf("NextRetryAt(retryCount=%d) = +%v, want +%v", c.retryCount, got, c.wantOff)
			}
		}
	})
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func countKinds(events []Event) (retries, expires int) {
	for _, ev := range events {
		switch ev.Kind {
		case EventRetry:
			retries++
		case EventExpire:
			expires++
		}
	}
	return
}

func findExpire(events []Event) Event {
	for _, ev := range events {
		if ev.Kind == EventExpire {
			return ev
		}
	}
	return Event{}
}
