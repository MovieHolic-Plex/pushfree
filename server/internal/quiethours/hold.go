package quiethours

import (
	"context"
	"sync"
	"time"
)

// HeldMessage is a deferred live delivery. The message row is already
// persisted (todo 8 CreateFanout); this record tracks that live delivery to
// RecipientUserID is withheld until ReleaseAt.
type HeldMessage struct {
	MessageID       int64
	RecipientUserID int64
	ReleaseAt       time.Time
}

// HoldStore tracks deferred live deliveries and releases them when due. The
// default in-memory implementation (MemoryHoldStore) is the todo-14 stand-in;
// todo 22 backs this with the durable timers table (Kind="quiethours") without
// changing this interface.
type HoldStore interface {
	// Add records a hold. Duplicate MessageIDs are allowed (idempotent at the
	// caller level); the caller gates Add on Evaluate == Hold.
	Add(ctx context.Context, hm HeldMessage) error
	// ReleaseDue removes and returns every hold whose ReleaseAt <= now, in
	// ReleaseAt order. Each hold is returned to exactly one caller.
	ReleaseDue(ctx context.Context, now time.Time) ([]HeldMessage, error)
	// Pending returns the unreleased holds for one recipient, in ReleaseAt
	// order. Used for observability and per-recipient independence tests.
	Pending(ctx context.Context, recipientUserID int64) ([]HeldMessage, error)
}

// MemoryHoldStore is the in-process, non-durable HoldStore. It is safe for
// concurrent use. It is the todo-14 stand-in pending the durable timer engine
// (todo 22); see doc.go for the migration seam.
type MemoryHoldStore struct {
	mu   sync.Mutex
	held []HeldMessage
}

// NewMemoryHoldStore returns an empty in-memory HoldStore.
func NewMemoryHoldStore() *MemoryHoldStore { return &MemoryHoldStore{} }

// Add appends a hold. The context is accepted for interface symmetry but is
// not consulted (the operation cannot block).
func (s *MemoryHoldStore) Add(_ context.Context, hm HeldMessage) error {
	s.mu.Lock()
	s.held = append(s.held, hm)
	s.mu.Unlock()
	return nil
}

// ReleaseDue removes and returns every hold with ReleaseAt <= now, leaving the
// rest in place, ordered by ReleaseAt then MessageID.
func (s *MemoryHoldStore) ReleaseDue(_ context.Context, now time.Time) ([]HeldMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.held[:0]
	var due []HeldMessage
	for _, h := range s.held {
		if !h.ReleaseAt.After(now) { // ReleaseAt <= now
			due = append(due, h)
			continue
		}
		kept = append(kept, h)
	}
	// Detach kept from the underlying slice so future appends do not alias a
	// shrunken re-slice of the old array.
	out := make([]HeldMessage, len(kept))
	copy(out, kept)
	s.held = out
	sortHolds(due)
	return due, nil
}

// Pending returns the unreleased holds for recipientUserID, ordered by
// ReleaseAt then MessageID.
func (s *MemoryHoldStore) Pending(_ context.Context, recipientUserID int64) ([]HeldMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []HeldMessage
	for _, h := range s.held {
		if h.RecipientUserID == recipientUserID {
			out = append(out, h)
		}
	}
	sortHolds(out)
	return out, nil
}

// sortHolds orders holds by ReleaseAt then MessageID for deterministic output.
func sortHolds(h []HeldMessage) {
	for i := 1; i < len(h); i++ {
		for j := i; j > 0; j-- {
			a, b := h[j-1], h[j]
			if a.ReleaseAt.Before(b.ReleaseAt) {
				break
			}
			if a.ReleaseAt.Equal(b.ReleaseAt) && a.MessageID <= b.MessageID {
				break
			}
			h[j-1], h[j] = b, a
		}
	}
}

// ReleaseFunc is invoked by Manager.Run with the holds released in a scan. It
// is the seam at which the fanout layer re-publishes to the hub.
type ReleaseFunc func(due []HeldMessage)
