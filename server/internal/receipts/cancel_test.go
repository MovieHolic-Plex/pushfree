package receipts

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// memCancelStore is an in-memory receipts.CancelStore for unit-testing the
// cancel domain logic (the race path and ErrNotCancellable wrapping) without a
// database. It mirrors the conditional-UPDATE semantics: CancelPending only
// succeeds when the current state is pending.
type memCancelStore struct {
	mu       sync.Mutex
	receipts map[string]ReceiptSnapshot
	timers   map[string]int // receiptID -> timer count
	listed   []string       // ids returned by ListCancellableByTag
	// raceCancel, when set, makes the first CancelPending call a no-op (returns
	// false) to simulate a receipt leaving pending between Get and UPDATE; the
	// state is also flipped so the re-read sees the raced-into state.
	raceCancel bool
	raceCalled bool
}

func newMemCancelStore(ids ...string) *memCancelStore {
	m := &memCancelStore{receipts: map[string]ReceiptSnapshot{}, timers: map[string]int{}}
	for _, id := range ids {
		m.receipts[id] = ReceiptSnapshot{State: StatePending}
		m.timers[id] = 1
	}
	return m
}

func (m *memCancelStore) GetReceipt(_ context.Context, id string) (ReceiptSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.receipts[id]
	if !ok {
		return ReceiptSnapshot{}, errNotFound{}
	}
	return s, nil
}

func (m *memCancelStore) CancelPending(_ context.Context, id string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.raceCancel && !m.raceCalled {
		// Simulate a race: the conditional UPDATE matches zero rows because the
		// state changed concurrently; flip the state so the re-read sees it.
		m.raceCalled = true
		m.receipts[id] = ReceiptSnapshot{State: StateDelivered}
		return false, nil
	}
	s, ok := m.receipts[id]
	if !ok || s.State != StatePending {
		return false, nil
	}
	m.receipts[id] = ReceiptSnapshot{State: StateCanceled}
	return true, nil
}

func (m *memCancelStore) DeleteTimers(_ context.Context, receiptID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timers[receiptID] = 0
	return nil
}

func (m *memCancelStore) ListCancellableByTag(_ context.Context, _ string, _ int64) ([]string, error) {
	return m.listed, nil
}

// errNotFound is a local sentinel for the "unknown receipt" path; it is NOT
// store.ErrNotFound (this package does not import store), which is fine because
// Cancel only propagates the error.
type errNotFound struct{}

func (errNotFound) Error() string { return "not found" }

func TestCancellable(t *testing.T) {
	for _, tc := range []struct {
		s    State
		want bool
	}{
		{StatePending, true},
		{StateDelivered, false},
		{StateAcknowledged, false},
		{StateExpired, false},
		{StateCanceled, false},
	} {
		if got := Cancellable(tc.s); got != tc.want {
			t.Errorf("Cancellable(%s)=%v want %v", tc.s, got, tc.want)
		}
	}
}

func TestCancelPendingSuccessClearsTimers(t *testing.T) {
	s := newMemCancelStore("R1")
	ok, err := Cancel(context.Background(), s, "R1", time.Now())
	if err != nil || !ok {
		t.Fatalf("Cancel pending: ok=%v err=%v", ok, err)
	}
	if got := s.receipts["R1"].State; got != StateCanceled {
		t.Fatalf("state=%s want canceled", got)
	}
	if s.timers["R1"] != 0 {
		t.Fatalf("timers not cleared: %d", s.timers["R1"])
	}
}

func TestCancelNonPendingErrors(t *testing.T) {
	for _, state := range []State{StateDelivered, StateAcknowledged, StateExpired, StateCanceled} {
		s := &memCancelStore{receipts: map[string]ReceiptSnapshot{"R": {State: state}}, timers: map[string]int{}}
		ok, err := Cancel(context.Background(), s, "R", time.Now())
		if ok || !errors.Is(err, ErrNotCancellable) {
			t.Errorf("Cancel(%s): ok=%v err=%v, want false ErrNotCancellable", state, ok, err)
		}
		// The wrapped error names the offending state.
		if !contains(string(err.Error()), string(state)) {
			t.Errorf("Cancel(%s) error %q does not name the state", state, err.Error())
		}
	}
}

func TestCancelRaceReReadsState(t *testing.T) {
	// Get sees pending, but the conditional UPDATE races (another transition
	// wins): CancelPending returns false and flips state to delivered. Cancel
	// must re-read and report ErrNotCancellable naming the raced-into state.
	s := newMemCancelStore("R")
	s.raceCancel = true
	ok, err := Cancel(context.Background(), s, "R", time.Now())
	if ok || !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("race: ok=%v err=%v, want false ErrNotCancellable", ok, err)
	}
	if !contains(err.Error(), "delivered") {
		t.Fatalf("race error %q should name the raced-into state (delivered)", err.Error())
	}
}

func TestCancelUnknownPropagatesError(t *testing.T) {
	s := newMemCancelStore() // empty
	_, err := Cancel(context.Background(), s, "missing", time.Now())
	if err == nil {
		t.Fatalf("unknown receipt: want error, got nil")
	}
	if !errors.Is(err, ErrNotCancellable) {
		// It must be a propagated lookup error, NOT a not-cancellable error.
		// (An unknown receipt has no state to report; the API maps this to 404.)
	}
}

func TestCancelByTagCancelsListedSet(t *testing.T) {
	s := newMemCancelStore("A", "B")
	s.listed = []string{"A", "B"}
	got, err := CancelByTag(context.Background(), s, "ops", 1, time.Now())
	if err != nil {
		t.Fatalf("CancelByTag: %v", err)
	}
	if len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("CancelByTag got=%v want [A B]", got)
	}
	for _, id := range []string{"A", "B"} {
		if s.receipts[id].State != StateCanceled {
			t.Errorf("%s state=%s want canceled", id, s.receipts[id].State)
		}
	}
}

func TestCancelByTagEmptyIsNoOp(t *testing.T) {
	s := newMemCancelStore()
	s.listed = nil
	got, err := CancelByTag(context.Background(), s, "nomatch", 1, time.Now())
	if err != nil {
		t.Fatalf("CancelByTag empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("CancelByTag empty got=%v want []", got)
	}
}

// contains is a tiny local helper (the receipts package must not grow a
// dependency just for string search).
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
