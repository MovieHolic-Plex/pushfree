// Package receipts is implemented test-first (todo 20). This file is the
// RED phase: the full receipt state-machine matrix, committed BEFORE any
// implementation, so the failing-first TDD discipline is auditable.
//
// It must FAIL to compile/run until statemachine.go and id.go exist.
package receipts

import (
	"errors"
	"regexp"
	"testing"
)

// allStates is the complete lifecycle state set in canonical order.
var allStates = []State{
	StatePending,
	StateDelivered,
	StateAcknowledged,
	StateExpired,
	StateCanceled,
}

// mustTransition fails the test if from->to is not a legal transition, else
// returns the resulting state. Centralises the legal-step assertion so the
// happy-path scenario reads as a raw sequence.
func mustTransition(t *testing.T, from, to State) State {
	t.Helper()
	next, err := Transition(from, to)
	if err != nil {
		t.Fatalf("Transition(%s->%s): unexpected error %v", from, to, err)
	}
	if next != to {
		t.Fatalf("Transition(%s->%s): got %s, want %s", from, to, next, to)
	}
	return next
}

// TestStateMachine is the acceptance entry point (todo 20). Its subtests
// cover the COMPLETE legal/illegal transition matrix, terminal states,
// idempotent ack, the explicitly-called-out illegal transitions, the happy
// path, and receipt-id format.
func TestStateMachine(t *testing.T) {
	// expectedLegal is the single declared source of truth the implementation
	// must mirror. The full-matrix subtest asserts every one of the 5x5=25
	// (from,to) pairs agrees with this set, so no transition is left ambiguous.
	//
	//   pending     -> {delivered, acknowledged, expired, canceled}
	//   delivered   -> {delivered (idempotent), acknowledged, expired, canceled}
	//   acknowledged-> {acknowledged}  (terminal; ack idempotent)
	//   expired     -> {expired}        (terminal)
	//   canceled    -> {canceled}       (terminal)
	expectedLegal := map[[2]State]bool{
		{StatePending, StateDelivered}:         true,
		{StatePending, StateAcknowledged}:      true,
		{StatePending, StateExpired}:           true,
		{StatePending, StateCanceled}:          true,
		{StateDelivered, StateDelivered}:       true, // idempotent re-delivery
		{StateDelivered, StateAcknowledged}:    true,
		{StateDelivered, StateExpired}:         true,
		{StateDelivered, StateCanceled}:        true,
		{StateAcknowledged, StateAcknowledged}: true, // idempotent ack
		{StateExpired, StateExpired}:           true, // terminal idempotent
		{StateCanceled, StateCanceled}:         true, // terminal idempotent
	}

	// FullMatrix: every (from,to) pair. Legal pairs must transition cleanly to
	// `to`; illegal pairs must be rejected with ErrIllegalTransition AND leave
	// the state unchanged (returned state == from).
	t.Run("FullMatrix", func(t *testing.T) {
		for _, from := range allStates {
			for _, to := range allStates {
				pair := [2]State{from, to}
				wantLegal := expectedLegal[pair]

				if got := CanTransition(from, to); got != wantLegal {
					t.Errorf("CanTransition(%s->%s)=%v, want %v", from, to, got, wantLegal)
				}

				next, err := Transition(from, to)
				if wantLegal {
					if err != nil || next != to {
						t.Errorf("Transition(%s->%s)=(%s,%v), want (%s,nil)",
							from, to, next, err, to)
					}
				} else {
					// Illegal: error must be ErrIllegalTransition and the
					// receipt state must be UNCHANGED (still `from`).
					if !errors.Is(err, ErrIllegalTransition) {
						t.Errorf("Transition(%s->%s): err=%v, want ErrIllegalTransition",
							from, to, err)
					}
					if next != from {
						t.Errorf("Transition(%s->%s): state changed to %s, must stay %s",
							from, to, next, from)
					}
				}
			}
		}
	})

	// HappyPath: the manual-QA raw sequence pending->delivered->acknowledged
	// must succeed step by step.
	t.Run("HappyPath", func(t *testing.T) {
		r := StatePending
		r = mustTransition(t, r, StateDelivered)
		r = mustTransition(t, r, StateAcknowledged)
		if r != StateAcknowledged {
			t.Fatalf("happy path ended at %s, want acknowledged", r)
		}
	})

	// Explicitly called out by the spec: acknowledged->expired is REJECTED and
	// leaves the state unchanged. (Also covered by FullMatrix; this is the
	// failure-case manual-QA assertion, raw and unambiguous.)
	t.Run("AcknowledgedToExpiredRejected", func(t *testing.T) {
		next, err := Transition(StateAcknowledged, StateExpired)
		if !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("acknowledged->expired: err=%v, want ErrIllegalTransition", err)
		}
		if next != StateAcknowledged {
			t.Fatalf("acknowledged->expired: state changed to %s, must stay acknowledged", next)
		}
	})

	// TerminalStates: acknowledged, expired, canceled are terminal. A receipt
	// in any of them can NEVER reach a different state; only an idempotent
	// self-loop is permitted.
	t.Run("TerminalStates", func(t *testing.T) {
		terminals := []State{StateAcknowledged, StateExpired, StateCanceled}
		for _, term := range terminals {
			if !term.Terminal() {
				t.Errorf("%s.Terminal()=false, want true", term)
			}
			for _, target := range allStates {
				if target == term {
					continue // self-loop is the only legal move (idempotent)
				}
				next, err := Transition(term, target)
				if !errors.Is(err, ErrIllegalTransition) {
					t.Errorf("terminal %s->%s: err=%v, want ErrIllegalTransition",
						term, target, err)
				}
				if next != term {
					t.Errorf("terminal %s->%s: state changed to %s, must stay %s",
						term, target, next, term)
				}
			}
		}
		// pending and delivered are NOT terminal.
		if StatePending.Terminal() {
			t.Errorf("pending.Terminal()=true, want false")
		}
		if StateDelivered.Terminal() {
			t.Errorf("delivered.Terminal()=true, want false")
		}
	})

	// CanceledIsTerminal: cancel is final. Any attempt to leave `canceled`
	// (except re-cancel) is rejected and leaves state unchanged.
	t.Run("CanceledIsTerminal", func(t *testing.T) {
		next, err := Transition(StateCanceled, StatePending)
		if !errors.Is(err, ErrIllegalTransition) || next != StateCanceled {
			t.Errorf("canceled->pending: got (%s,%v), want (canceled,ErrIllegalTransition)",
				next, err)
		}
		// Re-cancel is the idempotent no-op and stays canceled.
		if n2, err2 := Transition(StateCanceled, StateCanceled); err2 != nil || n2 != StateCanceled {
			t.Errorf("canceled->canceled: got (%s,%v), want (canceled,nil)", n2, err2)
		}
	})

	// IdempotentAck: a second acknowledgement on an already-acknowledged
	// receipt returns the SAME result (acknowledged) with no error. This is
	// the Pushover ack idempotency contract.
	t.Run("IdempotentAck", func(t *testing.T) {
		r := mustTransition(t, mustTransition(t, StatePending, StateDelivered), StateAcknowledged)
		first, err := Transition(r, StateAcknowledged)
		if err != nil {
			t.Fatalf("first repeat ack: unexpected error %v", err)
		}
		second, err := Transition(first, StateAcknowledged)
		if err != nil {
			t.Fatalf("second repeat ack: unexpected error %v", err)
		}
		if first != StateAcknowledged || second != StateAcknowledged {
			t.Fatalf("idempotent ack: first=%s second=%s, want acknowledged for both",
				first, second)
		}
		if first != second {
			t.Fatalf("idempotent ack changed result: %s != %s", first, second)
		}
	})

	// IsIdempotent correctly classifies the idempotent self-loops.
	t.Run("IsIdempotentClassification", func(t *testing.T) {
		idempotent := []State{StateDelivered, StateAcknowledged, StateExpired, StateCanceled}
		for _, s := range idempotent {
			if !IsIdempotent(s, s) {
				t.Errorf("IsIdempotent(%s,%s)=false, want true", s, s)
			}
		}
		// pending->pending is NOT legal/idempotent (no self-loop).
		if IsIdempotent(StatePending, StatePending) {
			t.Errorf("IsIdempotent(pending,pending)=true, want false")
		}
		// Cross-state transitions are never idempotent.
		if IsIdempotent(StatePending, StateDelivered) {
			t.Errorf("IsIdempotent(pending,delivered)=true, want false")
		}
	})

	// UnknownState: an unrecognised State value yields ErrUnknownState and
	// leaves the state unchanged, guarding the store layer against corrupt
	// rows or caller bugs.
	t.Run("UnknownState", func(t *testing.T) {
		bogus := State("bogus")
		if next, err := Transition(StatePending, bogus); !errors.Is(err, ErrUnknownState) || next != StatePending {
			t.Errorf("pending->bogus: got (%s,%v), want (pending,ErrUnknownState)", next, err)
		}
		if next, err := Transition(bogus, StatePending); !errors.Is(err, ErrUnknownState) || next != bogus {
			t.Errorf("bogus->pending: got (%s,%v), want (bogus,ErrUnknownState)", next, err)
		}
		if bogus.Valid() {
			t.Errorf("State(%q).Valid()=true, want false", bogus)
		}
	})
}

// TestStateMachineIDFormat verifies NewID produces exactly 30 characters over
// [A-Za-z0-9], matching the receipts.id CHECK(length=30) constraint and the
// Pushover receipt-id format. Many iterations also assert uniqueness.
func TestStateMachineIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^[A-Za-z0-9]{30}$`)
	const n = 2000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID[%d]: unexpected error %v", i, err)
		}
		if len(id) != 30 {
			t.Fatalf("NewID[%d]: len=%d, want 30: %q", i, len(id), id)
		}
		if !re.MatchString(id) {
			t.Fatalf("NewID[%d]: %q does not match [A-Za-z0-9]{30}", i, id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID[%d]: collision %q after %d generations", i, id, i)
		}
		seen[id] = struct{}{}
	}
}
