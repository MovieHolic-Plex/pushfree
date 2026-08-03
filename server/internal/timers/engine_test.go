// This file is the RED phase of todo 22 (durable timer engine + crash
// recovery). It is committed BEFORE the implementation files engine.go /
// jitter.go / retry.go exist, so `go test ./internal/timers/...` fails to
// build (undefined Engine/Store/Handler/JitterDuration/NewRetryHandler
// symbols) until they land -- the failing-first discipline is auditable in
// the evidence file, mirroring todo 20/21.
//
// Every test drives a REAL SQLite database (temp file, WAL, modernc driver)
// through a faithful timers.Store impl defined below, so the atomic-claim
// guarantee is exercised against the database engine itself, not a mock. The
// ClaimDue SQL is the SAME single-statement UPDATE ... RETURNING used in
// production (internal/store/sqlite/timer.go); Delete and ResetOrphanedClaims
// mirror internal/store/sqlite/timer_engine.go.
//
// A self-contained store is used (rather than importing store/sqlite) so the
// engine's crash-recovery contract is validated without depending on other
// todos' concrete repos. The injected clock makes every due-comparison
// deterministic: there are NO real sleeps except the explicit poll tick in
// TestRunStopsOnCanceledContext, which is the one sleep the gate allows.
package timers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/pushfree/pushfree/internal/receipts"
	"github.com/pushfree/pushfree/internal/store"
)

// ---- faithful SQLite-backed timers.Store for tests -------------------------

const timersSchema = `
CREATE TABLE timers (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL CHECK(kind IN ('retry','expire','callback','quiethours')),
    receipt_id TEXT,
    fire_at    TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    claimed_at TEXT
);
CREATE INDEX idx_timers_due ON timers(fire_at) WHERE claimed_at IS NULL;
CREATE INDEX idx_timers_orphaned ON timers(id) WHERE claimed_at IS NOT NULL;
`

// sqlStore is a faithful timers.Store backed by a real SQLite database. Its
// ClaimDue is the single-statement atomic claim (UPDATE...RETURNING), the
// same SQL shape as the production adapter -- this is what makes the
// concurrent-claim and crash-recovery tests prove real database atomicity.
type sqlStore struct {
	db *sql.DB
}

func newSQLStore(t *testing.T) *sqlStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "timers-test.db")
	// _pragma foreign_keys=0: no FK parent rows in this isolated test schema.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(0)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(timersSchema); err != nil {
		t.Fatalf("create timers schema: %v", err)
	}
	return &sqlStore{db: db}
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t.UTC()
}

func (s *sqlStore) Create(ctx context.Context, in *store.Timer) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO timers(kind, receipt_id, fire_at, payload, claimed_at) VALUES (?, ?, ?, ?, ?)`,
		in.Kind, optStr(in.ReceiptID), fmtTime(in.FireAt), in.Payload, nil)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	in.ID = id
	return id, nil
}

func (s *sqlStore) ClaimDue(ctx context.Context, now time.Time, limit int) ([]store.Timer, error) {
	if limit <= 0 {
		limit = 100
	}
	nowStr := fmtTime(now)
	// Single-statement atomic claim: each timer handed to exactly one caller.
	rows, err := s.db.QueryContext(ctx, `
UPDATE timers SET claimed_at = ?
WHERE id IN (
	SELECT id FROM timers
	WHERE fire_at <= ? AND claimed_at IS NULL
	ORDER BY id
	LIMIT ?
)
RETURNING id, kind, receipt_id, fire_at, payload, claimed_at`,
		nowStr, nowStr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.Timer, 0, limit)
	for rows.Next() {
		var (
			tm        store.Timer
			receiptID sql.NullString
			fireAt    sql.NullString
			claimedAt sql.NullString
		)
		if err := rows.Scan(&tm.ID, &tm.Kind, &receiptID, &fireAt, &tm.Payload, &claimedAt); err != nil {
			return nil, err
		}
		if receiptID.Valid {
			tm.ReceiptID = receiptID.String
		}
		if fireAt.Valid {
			tm.FireAt = parseTime(fireAt.String)
		}
		if claimedAt.Valid {
			ct := parseTime(claimedAt.String)
			tm.ClaimedAt = &ct
		}
		out = append(out, tm)
	}
	return out, rows.Err()
}

func (s *sqlStore) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM timers WHERE id = ?`, id)
	return err
}

func (s *sqlStore) ResetOrphanedClaims(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE timers SET claimed_at = NULL WHERE claimed_at IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// countClaimed returns the number of rows with claimed_at non-null (orphans).
func (s *sqlStore) countClaimed(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM timers WHERE claimed_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count claimed: %v", err)
	}
	return n
}

// countAll returns total timer rows.
func (s *sqlStore) countAll(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM timers`).Scan(&n); err != nil {
		t.Fatalf("count all: %v", err)
	}
	return n
}

func optStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- shared test helpers ---------------------------------------------------

// fireRecorder is a concurrency-safe per-id fire counter. The exactly-once
// contract is: every id appears exactly once across the whole run.
type fireRecorder struct {
	mu   sync.Mutex
	hits map[int64]int
}

func newFireRecorder() *fireRecorder { return &fireRecorder{hits: make(map[int64]int)} }

func (r *fireRecorder) handler() Handler {
	return func(_ context.Context, t store.Timer) error {
		r.mu.Lock()
		r.hits[t.ID]++
		r.mu.Unlock()
		return nil
	}
}

func (r *fireRecorder) assertAllOnce(t *testing.T, wantIDs []int64) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.hits) != len(wantIDs) {
		t.Fatalf("distinct fired timers = %d, want %d; hits=%v", len(r.hits), len(wantIDs), r.hits)
	}
	total := 0
	for _, id := range wantIDs {
		n := r.hits[id]
		total += n
		if n != 1 {
			t.Fatalf("timer %d fired %d times, want exactly 1; hits=%v", id, n, r.hits)
		}
	}
	if total != len(wantIDs) {
		t.Fatalf("total fires = %d, want %d (exactly once each); hits=%v", total, len(wantIDs), r.hits)
	}
}

// makeDueTimers inserts n timers due at base with the given kind, returning
// their ids in insertion order.
func makeDueTimers(t *testing.T, s *sqlStore, kind string, n int, base time.Time) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		tm := store.Timer{Kind: kind, FireAt: base, Payload: fmt.Sprintf("p%d", i)}
		id, err := s.Create(context.Background(), &tm)
		if err != nil {
			t.Fatalf("create timer %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// ---- tests -----------------------------------------------------------------

// TestCrashRecoveryExactlyOnce is the todo-22 headline acceptance: N timers,
// the engine is force-killed mid-run (after a claim, before fire -- the
// explicit, deterministic kill point), restarted, and every timer must fire
// EXACTLY once. This is the kill -9 simulation, driven deterministically via
// the Claim/Fire split (no goroutine timing, no sleeps).
func TestCrashRecoveryExactlyOnce(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const N = 6
	ids := makeDueTimers(t, s, KindCallback, N, base)
	rec := newFireRecorder()
	ctx := context.Background()

	// --- Engine 1: claim batch1, fire+delete; claim batch2, then CRASH ---
	eng1 := NewEngine(s, WithBatch(2), WithClock(func() time.Time { return base }))
	eng1.RegisterHandler(KindCallback, rec.handler())

	batch1, err := eng1.Claim(ctx) // ids 1,2
	if err != nil {
		t.Fatalf("eng1 claim1: %v", err)
	}
	if len(batch1) != 2 {
		t.Fatalf("batch1 = %d timers, want 2", len(batch1))
	}
	if fired, err := eng1.Fire(ctx, batch1); err != nil || fired != 2 {
		t.Fatalf("eng1 fire1: fired=%d err=%v, want 2/nil", fired, err)
	}
	batch2, err := eng1.Claim(ctx) // ids 3,4
	if err != nil {
		t.Fatalf("eng1 claim2: %v", err)
	}
	if len(batch2) != 2 {
		t.Fatalf("batch2 = %d timers, want 2", len(batch2))
	}
	// *** KILL -9 SIMULATION ***: batch2 was claimed but is NEVER fired.
	// The process dies here. batch2 rows survive with claimed_at set
	// (orphans); ids 5,6 were never claimed.
	_ = batch2

	// Invariants at the crash point: 2 fired+deleted, 2 orphaned, 2 untouched.
	if got := s.countAll(t); got != 4 { // 6 - 2 deleted
		t.Fatalf("after crash: rows = %d, want 4 (2 orphaned + 2 untouched)", got)
	}
	if got := s.countClaimed(t); got != 2 {
		t.Fatalf("after crash: orphaned claimed = %d, want 2", got)
	}

	// --- Engine 2 (restart): Recover, then drive to completion ---
	eng2 := NewEngine(s, WithBatch(2), WithClock(func() time.Time { return base }))
	eng2.RegisterHandler(KindCallback, rec.handler())

	reset, err := eng2.Recover(ctx) // startup scan: reset the 2 orphans
	if err != nil {
		t.Fatalf("eng2 recover: %v", err)
	}
	if reset != 2 {
		t.Fatalf("recover reset %d orphans, want 2", reset)
	}
	if got := s.countClaimed(t); got != 0 {
		t.Fatalf("after recover: claimed = %d, want 0", got)
	}
	// Drive remaining work: 4 timers (2 recovered + 2 untouched), batch 2.
	for i := 0; i < 4; i++ {
		n, err := eng2.Poll(ctx)
		if err != nil {
			t.Fatalf("eng2 poll %d: %v", i, err)
		}
		if n == 0 {
			break
		}
	}
	if got := s.countAll(t); got != 0 {
		t.Fatalf("after restart drain: rows = %d, want 0 (all fired+deleted)", got)
	}
	rec.assertAllOnce(t, ids)
}

// TestConcurrentClaimExactlyOnce is the failure-case manual QA: two workers
// polling concurrently must never both receive the same timer. Under -race
// the atomic ClaimDue (single UPDATE...RETURNING under the SQLite write lock)
// hands each timer to exactly one worker; the shared fire counter must equal
// exactly 1 per timer.
func TestConcurrentClaimExactlyOnce(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const N = 200
	ids := makeDueTimers(t, s, KindCallback, N, base)
	rec := newFireRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	worker := func() {
		eng := NewEngine(s, WithBatch(10), WithClock(func() time.Time { return base }))
		eng.RegisterHandler(KindCallback, rec.handler())
		for {
			if ctx.Err() != nil {
				return
			}
			claimed, err := eng.Claim(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Errorf("worker claim: %v", err)
				return
			}
			if len(claimed) == 0 {
				return // drained
			}
			if _, err := eng.Fire(ctx, claimed); err != nil {
				t.Errorf("worker fire: %v", err)
				return
			}
		}
	}

	var wg sync.WaitGroup
	const workers = 2
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker() }()
	}
	wg.Wait()

	if got := s.countAll(t); got != 0 {
		t.Fatalf("rows left = %d, want 0", got)
	}
	rec.assertAllOnce(t, ids)
}

// TestStartupScanResumesWithNoDuplicates proves Recover is idempotent and
// never duplicates work: manually-orphaned claims reset exactly once and the
// following poll fires each exactly once.
func TestStartupScanResumesWithNoDuplicates(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ids := makeDueTimers(t, s, KindCallback, 3, base)
	ctx := context.Background()

	// Simulate 3 orphans from a crash: claim them directly (no fire).
	claimed, err := s.ClaimDue(ctx, base, 10)
	if err != nil {
		t.Fatalf("pre-claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("pre-claim = %d, want 3", len(claimed))
	}
	if got := s.countClaimed(t); got != 3 {
		t.Fatalf("orphans = %d, want 3", got)
	}

	eng := NewEngine(s, WithClock(func() time.Time { return base }))
	rec := newFireRecorder()
	eng.RegisterHandler(KindCallback, rec.handler())

	// Recover must reset all 3 and be idempotent (a second Recover resets 0).
	if n, err := eng.Recover(ctx); err != nil || n != 3 {
		t.Fatalf("recover #1: n=%d err=%v, want 3/nil", n, err)
	}
	if n, err := eng.Recover(ctx); err != nil || n != 0 {
		t.Fatalf("recover #2 (idempotent): n=%d err=%v, want 0/nil", n, err)
	}

	// Poll once fires all 3; a second poll fires none.
	if n, err := eng.Poll(ctx); err != nil || n != 3 {
		t.Fatalf("poll #1: n=%d err=%v, want 3/nil", n, err)
	}
	if n, err := eng.Poll(ctx); err != nil || n != 0 {
		t.Fatalf("poll #2 (drained): n=%d err=%v, want 0/nil", n, err)
	}
	rec.assertAllOnce(t, ids)
}

// TestFireThenDeleteLeavesNoSurvivingRow pins the fire-then-delete invariant:
// a fired timer's row is gone, so a surviving row always means pending work.
func TestFireThenDeleteLeavesNoSurvivingRow(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ids := makeDueTimers(t, s, KindCallback, 1, base)
	eng := NewEngine(s, WithClock(func() time.Time { return base }))
	eng.RegisterHandler(KindCallback, newFireRecorder().handler())

	if _, err := eng.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := s.countAll(t); got != 0 {
		t.Fatalf("rows after fire = %d, want 0 (fire-then-delete)", got)
	}
	_ = ids
}

// TestHandlerErrorLeavesClaimedNotDeleted proves a failed handler does not
// delete the row: it becomes an orphan Recover retries. This is the
// at-least-once retry path for handler failures.
func TestHandlerErrorLeavesClaimedNotDeleted(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	makeDueTimers(t, s, KindCallback, 1, base)
	ctx := context.Background()

	eng := NewEngine(s, WithClock(func() time.Time { return base }))
	eng.RegisterHandler(KindCallback, func(context.Context, store.Timer) error {
		return errors.New("boom")
	})

	claimed, err := eng.Claim(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	_, fErr := eng.Fire(ctx, claimed)
	if fErr == nil {
		t.Fatalf("Fire: want error from failing handler, got nil")
	}
	// Row survives, claimed (orphan) -- not deleted.
	if got := s.countAll(t); got != 1 {
		t.Fatalf("rows after failed handler = %d, want 1 (kept for retry)", got)
	}
	if got := s.countClaimed(t); got != 1 {
		t.Fatalf("claimed after failed handler = %d, want 1", got)
	}
	// Recover + a succeeding handler retries it exactly once.
	eng.RegisterHandler(KindCallback, func(context.Context, store.Timer) error { return nil })
	if n, err := eng.Recover(ctx); err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v, want 1", n, err)
	}
	if n, err := eng.Poll(ctx); err != nil || n != 1 {
		t.Fatalf("retry poll: n=%d err=%v, want 1", n, err)
	}
	if got := s.countAll(t); got != 0 {
		t.Fatalf("rows after retry = %d, want 0", got)
	}
}

// TestPollHonorsContextCancel proves the onClaimed kill point (between claim
// and fire) leaves claimed rows as orphans for Recover -- the modeled crash
// via the production Poll path rather than the Claim/Fire split.
func TestPollHonorsContextCancel(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	ids := makeDueTimers(t, s, KindCallback, 2, base)
	ctx, cancel := context.WithCancel(context.Background())

	eng := NewEngine(s, WithBatch(2), WithClock(func() time.Time { return base }),
		WithOnClaimed(func([]store.Timer) { cancel() })) // kill between claim and fire
	eng.RegisterHandler(KindCallback, newFireRecorder().handler())

	if _, err := eng.Poll(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Poll after kill = %v, want context.Canceled", err)
	}
	// 2 claimed (orphans), none fired/deleted.
	if got := s.countClaimed(t); got != 2 {
		t.Fatalf("orphans after kill = %d, want 2", got)
	}
	if got := s.countAll(t); got != 2 {
		t.Fatalf("rows after kill = %d, want 2", got)
	}

	// Restart: Recover + Poll fires them once.
	eng2 := NewEngine(s, WithBatch(2), WithClock(func() time.Time { return base }))
	rec := newFireRecorder()
	eng2.RegisterHandler(KindCallback, rec.handler())
	if n, err := eng2.Recover(context.Background()); err != nil || n != 2 {
		t.Fatalf("recover: n=%d err=%v, want 2", n, err)
	}
	if n, err := eng2.Poll(context.Background()); err != nil || n != 2 {
		t.Fatalf("restart poll: n=%d err=%v, want 2", n, err)
	}
	rec.assertAllOnce(t, ids)
}

// TestRunStopsOnCanceledContext proves Run returns promptly when its context
// is already cancelled (no goroutine leak, no busy loop). This is the one
// place a real sleep exists (the poll tick); a pre-cancelled context makes it
// deterministic.
func TestRunStopsOnCanceledContext(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(s, WithClock(func() time.Time { return base }))
	eng.RegisterHandler(KindCallback, func(context.Context, store.Timer) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- eng.Run(ctx, 10*time.Millisecond) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s after cancel")
	}
}

// TestJitterWithinTenPercent asserts JitterDuration stays within ±10% of the
// nominal interval across many samples, with a seeded (deterministic) rng.
func TestJitterWithinTenPercent(t *testing.T) {
	t.Parallel()
	rng := newRand(1)
	d := 30 * time.Second
	lo, hi := jitterLowHigh(d)
	for i := 0; i < 10000; i++ {
		got := JitterDuration(d, rng)
		if got < lo || got > hi {
			t.Fatalf("sample %d: jitter = %v, want within [%v, %v]", i, got, lo, hi)
		}
	}
	// Nil rng = no jitter (exact).
	if got := JitterDuration(d, nil); got != d {
		t.Fatalf("nil-rng jitter = %v, want %v (no jitter)", got, d)
	}
}

// TestJitterCoversBothSidesOfNominal guards against an off-by-sign bug that
// always skews high (or low): across enough samples, both below and above the
// nominal must appear.
func TestJitterCoversBothSidesOfNominal(t *testing.T) {
	t.Parallel()
	rng := newRand(7)
	const d = 30 * time.Second
	var below, above int
	for i := 0; i < 1000; i++ {
		switch j := JitterDuration(d, rng); {
		case j < d:
			below++
		case j > d:
			above++
		}
	}
	if below == 0 || above == 0 {
		t.Fatalf("jitter one-sided: below=%d above=%d (want both >0)", below, above)
	}
}

// TestRetryAdapterWiring is the todo-21->22 wiring: a fired "retry" timer
// drives one receipts.Scheduler.Tick, redelivers once, and schedules the next
// retry timer with ±10% jitter; when the receipt expires, no follow-up timer
// is scheduled.
func TestRetryAdapterWiring(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{cur: base}
	ctx := context.Background()

	eng := NewEngine(s, WithClock(clock.now()))
	const receiptID = "r1"
	retryStore := newRetryMemStore() // pending receipt
	var redelivered atomic.Int32
	redeliver := func(context.Context, string, int) error { redelivered.Add(1); return nil }
	rng := newRand(42)
	eng.RegisterHandler(KindRetry, NewRetryHandler(eng, retryStore,
		receipts.RetryPolicy{RetryInterval: 30 * time.Second, Expire: 90 * time.Second, MaxAttempts: 50},
		clock.now(), redeliver, rng))

	// Seed the first retry timer, due immediately at base.
	payload, _ := MarshalPayload(RetryPayload{ReceiptID: receiptID, CreatedAt: base})
	first := store.Timer{Kind: KindRetry, ReceiptID: receiptID, FireAt: base, Payload: payload}
	if _, err := s.Create(ctx, &first); err != nil {
		t.Fatalf("seed retry timer: %v", err)
	}

	// Fire attempt 1 at base. Redeliver fires once; exactly one follow-up
	// retry timer is scheduled at ~base+30s (±10%).
	if n, err := eng.Poll(ctx); err != nil || n != 1 {
		t.Fatalf("poll #1: n=%d err=%v, want 1/nil", n, err)
	}
	if got := redelivered.Load(); got != 1 {
		t.Fatalf("redeliver after attempt1 = %d, want 1", got)
	}
	next := mustSinglePendingTimer(t, s)
	lo, hi := jitterLowHigh(30 * time.Second)
	if next.FireAt.Before(base.Add(lo)) || next.FireAt.After(base.Add(hi)) {
		t.Fatalf("next retry fire_at = %v, want within [%v, %v] (±10%% of 30s)", next.FireAt, base.Add(lo), base.Add(hi))
	}

	// Fire attempt 2 at +30s. Redeliver now 2; a further timer is scheduled.
	clock.set(base.Add(30 * time.Second))
	if n, err := eng.Poll(ctx); err != nil || n != 1 {
		t.Fatalf("poll #2: n=%d err=%v, want 1/nil", n, err)
	}
	if got := redelivered.Load(); got != 2 {
		t.Fatalf("redeliver after attempt2 = %d, want 2", got)
	}
	mustSinglePendingTimer(t, s)

	// Fire attempt 3 at +90s: expire window (90s) has elapsed -> EventExpire,
	// receipt becomes expired, NO follow-up timer is scheduled.
	clock.set(base.Add(90 * time.Second))
	if n, err := eng.Poll(ctx); err != nil || n != 1 {
		t.Fatalf("poll #3 (expire): n=%d err=%v, want 1/nil", n, err)
	}
	if got := redelivered.Load(); got != 2 { // not incremented on expire
		t.Fatalf("redeliver after expire = %d, want 2 (no extra redelivery)", got)
	}
	if retryStore.state != receipts.StateExpired {
		t.Fatalf("receipt state = %s, want expired", retryStore.state)
	}
	if got := s.countAll(t); got != 0 {
		t.Fatalf("rows after expire = %d, want 0 (no follow-up timer)", got)
	}
}

// TestRetryAdapterTerminalReceiptNoFollowup proves an already-terminal receipt
// (e.g. acknowledged externally) yields no follow-up timer -- EventDone.
func TestRetryAdapterTerminalReceiptNoFollowup(t *testing.T) {
	t.Parallel()
	s := newSQLStore(t)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(s, WithClock(func() time.Time { return base }))
	retryStore := newRetryMemStore()
	retryStore.state = receipts.StateAcknowledged // terminal before any fire
	eng.RegisterHandler(KindRetry, NewRetryHandler(eng, retryStore,
		receipts.DefaultRetryPolicy(),
		func() time.Time { return base },
		func(context.Context, string, int) error { return errors.New("must not redeliver terminal") },
		newRand(1)))

	payload, _ := MarshalPayload(RetryPayload{ReceiptID: "rT", CreatedAt: base})
	if _, err := s.Create(context.Background(), &store.Timer{
		Kind: KindRetry, ReceiptID: "rT", FireAt: base, Payload: payload,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, err := eng.Poll(context.Background()); err != nil || n != 1 {
		t.Fatalf("poll: n=%d err=%v, want 1/nil", n, err)
	}
	if got := s.countAll(t); got != 0 {
		t.Fatalf("rows after terminal fire = %d, want 0 (no follow-up)", got)
	}
}

// jitterLowHigh returns the [low, high] bounds a ±10%-jittered d may take,
// computed the same way JitterDuration does (float multiply then trunc to
// Duration) so test bounds match the implementation exactly.
func jitterLowHigh(d time.Duration) (time.Duration, time.Duration) {
	lo := float64(d) * (1 - JitterFraction)
	hi := float64(d) * (1 + JitterFraction)
	return time.Duration(lo), time.Duration(hi)
}

// ---- small test helpers for retry wiring -----------------------------------

type fakeClock struct{ cur time.Time }

func (f *fakeClock) now() Clock      { return func() time.Time { return f.cur } }
func (f *fakeClock) set(t time.Time) { f.cur = t }

// retryMemStore is an in-memory receipts.RetryStore for the adapter test,
// mirroring the receipts scheduler_test.go memStore.
type retryMemStore struct {
	mu         sync.Mutex
	state      receipts.State
	retryCount int
}

func newRetryMemStore() *retryMemStore { return &retryMemStore{state: receipts.StatePending} }

func (m *retryMemStore) GetReceipt(context.Context, string) (receipts.ReceiptSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return receipts.ReceiptSnapshot{State: m.state, RetryCount: m.retryCount}, nil
}
func (m *retryMemStore) IncrementRetry(context.Context, string, time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retryCount++
	return nil
}
func (m *retryMemStore) SetExpired(context.Context, string, time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = receipts.StateExpired
	return nil
}

// mustSinglePendingTimer reads the one surviving unclaimed timer row, failing
// if there is not exactly one (the freshly-scheduled follow-up).
func mustSinglePendingTimer(t *testing.T, s *sqlStore) store.Timer {
	t.Helper()
	rows, err := s.db.Query(`SELECT id, kind, receipt_id, fire_at, payload, claimed_at FROM timers`)
	if err != nil {
		t.Fatalf("query pending: %v", err)
	}
	defer rows.Close()
	var (
		tm        store.Timer
		receiptID sql.NullString
		fireAt    sql.NullString
		claimedAt sql.NullString
		count     int
	)
	for rows.Next() {
		if err := rows.Scan(&tm.ID, &tm.Kind, &receiptID, &fireAt, &tm.Payload, &claimedAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if receiptID.Valid {
			tm.ReceiptID = receiptID.String
		}
		if fireAt.Valid {
			tm.FireAt = parseTime(fireAt.String)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("pending timers = %d, want exactly 1 follow-up", count)
	}
	return tm
}

// fmt import shim usage guard (keeps fmt used even if test bodies shrink).
var _ = fmt.Sprintf
