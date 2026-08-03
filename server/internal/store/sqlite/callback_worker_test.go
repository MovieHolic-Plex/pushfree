package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/callbacks"
	"github.com/pushfree/pushfree/internal/store"
)

// This file is the todo-25 integration check: the callbacks.Worker driven
// through the REAL SQLite CallbackWorkerRepo, asserting the persisted rows a
// production run would leave behind (receipts.called_back_at, callbacks.state,
// dlq history). It is the "raw" evidence for MANUAL QA -- happy path
// (ack->2xx->called_back_at set) and failure path (permanent 500 -> 60s
// retries + dlq rows). The logic-level contract is in
// internal/callbacks/worker_test.go; this test only pins the persistence.

// testClock is a controllable clock for the worker over the real store.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// seedCallbackTarget inserts a user, app, a priority-2 send carrying
// callbackURL, and an acknowledged receipt, returning the receipt id. The
// receipt.send_id FK links the receipt to the send (so GetTarget can join).
func seedCallbackTarget(t *testing.T, s *Store, callbackURL string) string {
	t.Helper()
	ctx := context.Background()
	sender := mustSeedUser(t, s.users, 60, "cw@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 61)
	ackAt := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	r := &store.Receipt{
		ID: key30(70), State: "acknowledged",
		AcknowledgedAt: &ackAt, AcknowledgedBy: "uk" + key30(71)[:28],
	}
	if _, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send: store.Send{
			AppID: app.ID, SenderUserID: sender.ID, Priority: 2,
			Body: "boom", CallbackURL: callbackURL, CreatedAt: ackAt,
		},
		Receipt: r,
	}); err != nil {
		t.Fatalf("seed fanout: %v", err)
	}
	return r.ID
}

func receiptCalledBackAt(t *testing.T, db *sql.DB, receiptID string) *time.Time {
	t.Helper()
	var null sql.NullString
	if err := db.QueryRow(`SELECT called_back_at FROM receipts WHERE id = ?`, receiptID).Scan(&null); err != nil {
		t.Fatalf("select called_back_at: %v", err)
	}
	return nullTime(null)
}

func callbackState(t *testing.T, db *sql.DB, receiptID string) (state string, attempts int, next *time.Time) {
	t.Helper()
	var nextNs sql.NullString
	if err := db.QueryRow(
		`SELECT state, attempts, next_attempt_at FROM callbacks WHERE receipt_id = ?`,
		receiptID).Scan(&state, &attempts, &nextNs); err != nil {
		t.Fatalf("select callback: %v", err)
	}
	return state, attempts, nullTime(nextNs)
}

func dlqCount(t *testing.T, db *sql.DB, receiptID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM dlq d JOIN callbacks c ON d.callback_id = c.id WHERE c.receipt_id = ?`,
		receiptID).Scan(&n); err != nil {
		t.Fatalf("select dlq count: %v", err)
	}
	return n
}

// allowedServerHost returns the host:port + bare host of an httptest server,
// so the worker permits its loopback address (blocked by default).
func allowedServerHost(srv *httptest.Server) []string {
	u, _ := url.Parse(srv.URL)
	return []string{u.Host, u.Hostname()}
}

// TestCallbackWorkerRepo_HappyPath exercises ack -> 2xx -> called_back_at over
// the real SQLite store: the receipt row ends up with a non-null called_back_at
// and the callback row is terminal 'done'.
func TestCallbackWorkerRepo_HappyPath(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)

	var calls int32
	var sawBody bool
	var bodyMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		sawBody = bytes.Contains(b, []byte("\"receipt\""))
		bodyMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	receiptID := seedCallbackTarget(t, s, srv.URL)
	clk := &testClock{t: time.Date(2026, 8, 3, 12, 0, 5, 0, time.UTC)}
	w := callbacks.NewWorker(NewCallbackWorkerRepo(s.DB()), callbacks.Options{
		Clock:        clk.now,
		AllowedHosts: allowedServerHost(srv),
	})

	if _, err := w.Enqueue(ctx, receiptID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := w.ProcessDue(ctx); err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server calls = %d, want 1", got)
	}
	bodyMu.Lock()
	if !sawBody {
		t.Errorf("POST body did not contain 'receipt' field")
	}
	bodyMu.Unlock()

	// RAW evidence: receipts.called_back_at is set.
	if cb := receiptCalledBackAt(t, s.DB(), receiptID); cb == nil {
		t.Fatalf("receipts.called_back_at is NULL, want a timestamp (happy path)")
	}
	// RAW evidence: callbacks row is terminal 'done'.
	if state, attempts, next := callbackState(t, s.DB(), receiptID); state != "done" {
		t.Fatalf("callback state = %q attempts=%d next=%v, want done", state, attempts, next)
	}
}

// TestCallbackWorkerRepo_PermanentFailure exercises a permanently-failing URL
// over the real store: 60s-interval retries continue, each failed attempt
// appends a dlq row, called_back_at stays NULL, and the callback is NOT
// aborted (remains retrying).
func TestCallbackWorkerRepo_PermanentFailure(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	receiptID := seedCallbackTarget(t, s, srv.URL)
	clk := &testClock{t: time.Date(2026, 8, 3, 12, 0, 5, 0, time.UTC)}
	w := callbacks.NewWorker(NewCallbackWorkerRepo(s.DB()), callbacks.Options{
		Clock:        clk.now,
		AllowedHosts: allowedServerHost(srv),
	})

	if _, err := w.Enqueue(ctx, receiptID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := w.ProcessDue(ctx); err != nil {
			t.Fatalf("ProcessDue #%d: %v", attempt, err)
		}
		if got := atomic.LoadInt32(&calls); got != int32(attempt) {
			t.Fatalf("after %d attempts, calls = %d", attempt, got)
		}
		// RAW evidence: one dlq row per failure.
		if n := dlqCount(t, s.DB(), receiptID); n != attempt {
			t.Fatalf("dlq rows after %d attempts = %d, want %d", attempt, n, attempt)
		}
		clk.add(60 * time.Second)
	}

	// RAW evidence: called_back_at stays NULL (never delivered).
	if cb := receiptCalledBackAt(t, s.DB(), receiptID); cb != nil {
		t.Fatalf("receipts.called_back_at = %v, want NULL (permanent failure)", cb)
	}
	// RAW evidence: still retrying (not aborted) with 3 attempts recorded.
	state, attempts, next := callbackState(t, s.DB(), receiptID)
	if state != "failed" || attempts != 3 || next == nil {
		t.Fatalf("callback = state=%q attempts=%d next=%v, want failed/3/scheduled", state, attempts, next)
	}
}
