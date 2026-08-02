package quiethours

import (
	"context"
	"log/slog"
	"time"
)

// Manager ties evaluation to a HoldStore and an injected clock. It is the
// single object the send path and the window-end flush loop depend on.
type Manager struct {
	holds HoldStore
	now   func() time.Time
	log   *slog.Logger
}

// New builds a Manager. A nil clock defaults to time.Now; a nil logger to
// slog.Default. holds MUST be non-nil.
func New(holds HoldStore, clock func() time.Time, logger *slog.Logger) *Manager {
	if clock == nil {
		clock = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{holds: holds, now: clock, log: logger}
}

// Now returns the current injected-clock instant. Exposed so the send path
// uses the same clock as the release loop when calling Evaluate.
func (m *Manager) Now() time.Time { return m.now() }

// Hold records that MessageID for RecipientUserID is deferred until the end of
// the recipient's current quiet window. The release instant is computed from
// the injected clock. Returns ErrNotInWindow if the clock is currently outside
// the window (callers normally avoid this by gating on Evaluate == Hold).
func (m *Manager) Hold(ctx context.Context, messageID, recipientUserID int64, s Settings) error {
	rel, ok := ReleaseAt(m.now(), s)
	if !ok {
		return ErrNotInWindow
	}
	return m.holds.Add(ctx, HeldMessage{
		MessageID:       messageID,
		RecipientUserID: recipientUserID,
		ReleaseAt:       rel,
	})
}

// ReleaseDue removes and returns every hold whose window has ended at the
// injected clock's current instant. The caller (the flush loop) re-publishes
// each returned hold to the hub.
func (m *Manager) ReleaseDue(ctx context.Context) ([]HeldMessage, error) {
	return m.holds.ReleaseDue(ctx, m.now())
}

// Pending returns the unreleased holds for one recipient.
func (m *Manager) Pending(ctx context.Context, recipientUserID int64) ([]HeldMessage, error) {
	return m.holds.Pending(ctx, recipientUserID)
}

// Run is the window-end flush loop: it polls ReleaseDue every poll (default
// 1m) and invokes release for each non-empty batch. It blocks until ctx is
// canceled. poll is the production cadence only; the decision clock is the
// injected one, so Run MUST be exercised through the synchronous Manager
// methods in tests rather than by sleeping on the ticker. A nil release is
// allowed and drops released holds after recording them (used only when the
// caller wants the store drained without a hub).
func (m *Manager) Run(ctx context.Context, poll time.Duration, release ReleaseFunc) {
	if poll <= 0 {
		poll = time.Minute
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			due, err := m.ReleaseDue(ctx)
			if err != nil {
				m.log.Warn("quiethours: release scan", "err", err)
				continue
			}
			if len(due) > 0 && release != nil {
				release(due)
			}
		}
	}
}
