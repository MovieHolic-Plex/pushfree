package retention

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/store/sqlite"
)

// fakeClock returns a fixed time settable by tests, so the sweeper's age math
// is deterministic without any real-time sleeping.
type fakeClock struct{ t time.Time }

func (f fakeClock) Now() time.Time { return f.t }

// key30 returns a deterministic 30-char string for a seed id (satisfies the
// length=30 CHECK on user_key and token).
func key30(n int) string { return fmt.Sprintf("%030d", n) }

// newStoreDB opens a fresh temp-file SQLite database for the sweeper to act on.
func newStoreDB(t *testing.T) *sqlite.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := sqlite.Open(context.Background(), filepath.Join(dir, "sweeper.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedUserApp(t *testing.T, s *sqlite.Store) (store.User, store.App) {
	t.Helper()
	u := store.User{
		Email: "sweep@example.com", PassHash: "h", Role: "user", UserKey: key30(11),
		QuietTZ: "UTC", CreatedAt: time.Now(),
	}
	uid, err := s.Repos().Users.Create(context.Background(), &u)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	u.ID = uid
	a := store.App{UserID: uid, Token: key30(22), Name: "a"}
	aid, err := s.Repos().Apps.Create(context.Background(), &a)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	a.ID = aid
	return u, a
}

// TestSweeperSweepOnceInjectedClock drives a single sweep at a fixed clock
// time and verifies all three passes (message retention, attachment BLOB
// retention, TTL discard) fire correctly with NO real-time sleeping.
func TestSweeperSweepOnceInjectedClock(t *testing.T) {
	ctx := context.Background()
	s := newStoreDB(t)
	user, app := seedUserApp(t, s)

	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	clk := fakeClock{t: now}

	// (a) old message past 30d retention.
	if _, err := s.Repos().Sends.CreateFanout(ctx, &store.Fanout{
		Send:     store.Send{AppID: app.ID, SenderUserID: user.ID, Body: "old-msg", CreatedAt: now.Add(-31 * 24 * time.Hour)},
		Messages: []store.Message{{RecipientUserID: user.ID, CreatedAt: now.Add(-31 * 24 * time.Hour)}},
	}); err != nil {
		t.Fatalf("seed old msg: %v", err)
	}
	// (b) old undownloaded attachment past 3d (BLOB cleared, row kept).
	oldAttSend, err := s.Repos().Sends.CreateFanout(ctx, &store.Fanout{
		Send: store.Send{AppID: app.ID, SenderUserID: user.ID, Body: "old-att", CreatedAt: now.Add(-4 * 24 * time.Hour)},
	})
	if err != nil {
		t.Fatalf("seed old att send: %v", err)
	}
	if _, err := s.Repos().Attachments.Create(ctx, &store.Attachment{
		SendID: oldAttSend, ContentType: "image/png", Data: []byte("PAYLOAD"),
	}); err != nil {
		t.Fatalf("seed old att: %v", err)
	}
	// (c) ttl-elapsed undelivered message (ttl=60s, created 2m ago).
	if _, err := s.Repos().Sends.CreateFanout(ctx, &store.Fanout{
		Send:     store.Send{AppID: app.ID, SenderUserID: user.ID, Body: "ttl", TTL: 60, CreatedAt: now.Add(-2 * time.Minute)},
		Messages: []store.Message{{RecipientUserID: user.ID, CreatedAt: now.Add(-2 * time.Minute)}},
	}); err != nil {
		t.Fatalf("seed ttl msg: %v", err)
	}

	sw, err := NewSweeper(s, clk, "1h", "720h", "72h", slog.Default())
	if err != nil {
		t.Fatalf("NewSweeper: %v", err)
	}

	stats, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if stats.MessagesDeleted != 1 {
		t.Errorf("MessagesDeleted = %d, want 1", stats.MessagesDeleted)
	}
	if stats.AttachmentBlobsCleared != 1 {
		t.Errorf("AttachmentBlobsCleared = %d, want 1", stats.AttachmentBlobsCleared)
	}
	if stats.TTLDiscarded != 1 {
		t.Errorf("TTLDiscarded = %d, want 1", stats.TTLDiscarded)
	}

	// Attachment row survived, BLOB gone.
	att, err := s.Repos().Attachments.GetBySendID(ctx, oldAttSend)
	if err != nil {
		t.Fatalf("GetBySendID: %v", err)
	}
	if len(att.Data) != 0 {
		t.Errorf("attachment BLOB not cleared by sweeper: len=%d", len(att.Data))
	}
}

// TestSweeperRunStopsOnCancel verifies Run returns promptly when its context
// is canceled, with no real-hour sleeping (interval is sub-second; the only
// thing under test is cancellation responsiveness).
func TestSweeperRunStopsOnCancel(t *testing.T) {
	s := newStoreDB(t)
	sw, err := NewSweeper(s, fakeClock{t: time.Now()}, "50ms", "720h", "72h", slog.Default())
	if err != nil {
		t.Fatalf("NewSweeper: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sw.Run(ctx) }()

	cancel()
	select {
	case <-done:
		// ok: Run returned on cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("Sweeper.Run did not stop within 2s of cancel")
	}
}

// TestSweeperRejectsBadDurations confirms startup fails loudly on a malformed
// duration (fail-fast at construction, not at first sweep).
func TestSweeperRejectsBadDurations(t *testing.T) {
	s := newStoreDB(t)
	cases := map[string]string{
		"interval":   "not-a-duration",
		"messages":   "720h",
		"attachment": "72h",
	}
	// Bad interval.
	if _, err := NewSweeper(s, fakeClock{}, cases["interval"], cases["messages"], cases["attachment"], slog.Default()); err == nil {
		t.Fatal("expected error for bad interval, got nil")
	}
	// Zero interval is invalid (sweeper must tick).
	if _, err := NewSweeper(s, fakeClock{}, "0", "720h", "72h", slog.Default()); err == nil {
		t.Fatal("expected error for zero interval, got nil")
	}
	// Bad messages-retention.
	if _, err := NewSweeper(s, fakeClock{}, "1h", "bad", "72h", slog.Default()); err == nil {
		t.Fatal("expected error for bad messages-retention, got nil")
	}
}
