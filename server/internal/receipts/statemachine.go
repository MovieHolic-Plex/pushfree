// Package receipts implements the priority-2 (emergency) receipt lifecycle
// as a pure state machine, plus receipt-id generation.
//
// Layering: this package owns the transition RULES and nothing else. The
// storage layer (internal/store/sqlite/receipt.go) persists rows and the
// receipts.send_id 1:1 is enforced by the schema's UNIQUE(send_id)
// constraint; the HTTP-facing ack/cancel/poll endpoints come later (todo
// 23+). Keeping the rules pure lets the full legal/illegal matrix be tested
// with zero database dependencies.
//
// This file was written test-first (todo 20): statemachine_test.go was
// committed RED before any code here existed.
package receipts

import "errors"

// State is a receipt lifecycle state. The string values are the exact tokens
// stored in receipts.state and constrained by CHECK(state IN (...)) in the
// schema, so they must never be renamed.
type State string

const (
	// StatePending is the initial state: receipt created, not yet delivered.
	StatePending State = "pending"
	// StateDelivered: at least one transport accepted delivery.
	StateDelivered State = "delivered"
	// StateAcknowledged: the recipient acknowledged (terminal).
	StateAcknowledged State = "acknowledged"
	// StateExpired: retry budget/expire timer exhausted before ack (terminal).
	StateExpired State = "expired"
	// StateCanceled: canceled via the API (terminal).
	StateCanceled State = "canceled"
)

var (
	// ErrIllegalTransition is returned by Transition when (from, to) is not a
	// legal state change. The receipt's state is left unchanged.
	ErrIllegalTransition = errors.New("receipts: illegal state transition")
	// ErrUnknownState is returned when from or to is not one of the five
	// defined states, guarding the store layer against corrupt rows or caller
	// bugs.
	ErrUnknownState = errors.New("receipts: unknown state")
)

// legalTransitions is the single source of truth for the receipt lifecycle.
// Every legal (from -> to) pair appears here; all other pairs are illegal.
//
//	pending     -> {delivered, acknowledged, expired, canceled}
//	delivered   -> {delivered (idempotent), acknowledged, expired, canceled}
//	acknowledged-> {acknowledged}  (terminal; ack is idempotent)
//	expired     -> {expired}        (terminal)
//	canceled    -> {canceled}       (terminal)
//
// Self-loops on the non-pending states model idempotent re-application of the
// same event (re-delivery, re-ack, re-expire, re-cancel) as a no-op. pending
// has no self-loop: "deliver to pending" is meaningless. A receipt may be
// acknowledged straight from pending (an ack from a device that has the
// message before the delivery-confirm hook records it) as well as from
// delivered.
var legalTransitions = map[State]map[State]struct{}{
	StatePending: {
		StateDelivered:    {},
		StateAcknowledged: {},
		StateExpired:      {},
		StateCanceled:     {},
	},
	StateDelivered: {
		StateDelivered:    {},
		StateAcknowledged: {},
		StateExpired:      {},
		StateCanceled:     {},
	},
	StateAcknowledged: {StateAcknowledged: {}},
	StateExpired:      {StateExpired: {}},
	StateCanceled:     {StateCanceled: {}},
}

// states is the full set of valid states, used for validation.
var states = map[State]struct{}{
	StatePending:      {},
	StateDelivered:    {},
	StateAcknowledged: {},
	StateExpired:      {},
	StateCanceled:     {},
}

// Valid reports whether s is one of the five defined receipt states.
func (s State) Valid() bool {
	_, ok := states[s]
	return ok
}

// Terminal reports whether s is terminal: once reached the receipt can never
// move to a different state (only an idempotent self-loop remains).
// Acknowledged, expired, and canceled are terminal; pending and delivered are
// not.
func (s State) Terminal() bool {
	switch s {
	case StateAcknowledged, StateExpired, StateCanceled:
		return true
	}
	return false
}

// CanTransition reports whether moving from -> to is a legal transition,
// including idempotent self-loops on delivered/acknowledged/expired/canceled.
// An unknown state yields false.
func CanTransition(from, to State) bool {
	targets, ok := legalTransitions[from]
	if !ok {
		return false
	}
	_, legal := targets[to]
	return legal
}

// IsIdempotent reports whether (from -> to) is a legal idempotent self-loop
// (re-delivery, re-ack, re-expire, re-cancel): a no-op that keeps the state.
// pending->pending and any cross-state transition are not idempotent.
func IsIdempotent(from, to State) bool {
	return from == to && from != "" && CanTransition(from, to)
}

// Transition applies from -> to. It returns the resulting state (== to) and a
// nil error when the transition is legal. For an illegal transition it returns
// (from, ErrIllegalTransition): the receipt's state is left unchanged. An
// unknown state yields (from, ErrUnknownState).
//
// This function is pure: callers apply the returned state to the persisted
// receipt. That separation keeps the rules testable without a database and
// lets the storage layer perform the conditional UPDATE atomically.
func Transition(from, to State) (State, error) {
	if !from.Valid() || !to.Valid() {
		return from, ErrUnknownState
	}
	if !CanTransition(from, to) {
		return from, ErrIllegalTransition
	}
	return to, nil
}
