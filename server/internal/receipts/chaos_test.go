// This file is the todo-26 acceptance suite: quota integrity + the kill -9
// chaos scenarios. It is a NEW file; product code is untouched (READ-ONLY
// integration tests).
//
// Every test drives the REAL SQLite store (temp file, WAL, modernc driver, the
// same *sqlite.Store production uses) and the REAL durable timer engine
// (internal/timers). The kill -9 model is the one the acceptance gate allows:
// work is abandoned mid-flight (an uncommitted transaction, or a
// claimed-but-not-fired timer) and the database handle is CLOSED, then the
// SAME temp file is REOPENED -- the exact recovery path a real process restart
// walks (SQLite WAL recovery + the timer engine's startup ResetOrphanedClaims
// scan). There are NO real sleeps, polling delays, or wait-for-time patterns:
// every time comparison goes through an injected clock.
//
// The suite is collected by `go test ./internal/... -run TestChaos` (every test
// is named TestChaos*).
package receipts_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver

	"github.com/pushfree/pushfree/internal/quota"
	"github.com/pushfree/pushfree/internal/receipts"
	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/store/sqlite"
	"github.com/pushfree/pushfree/internal/timers"
)

// ---- shared chaos helpers --------------------------------------------------

// chaosClock is an injectable clock shared by the timer engine and the retry
// scheduler so every fire_at / NextRetryAt comparison is deterministic. It
// persists across engine instances (it is the process wall clock stand-in), so
// a restarted engine sees the same "now" the crashed one set.
type chaosClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *chaosClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *chaosClock) set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

// timerClock returns a timers.Clock (func() time.Time) reading this clock.
func (c *chaosClock) timerClock() timers.Clock { return c.get }

// newChaosDB opens a fresh temp-file SQLite store with migrations applied and
// returns the store and its path (so the crash model can reopen the same file).
func newChaosDB(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "chaos.db")
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return s, path
}

// reopenChaosDB reopens the same temp file -- the restart half of the kill -9
// model. Migrations are idempotent (schema_migrations), so this is a no-op on
// an already-migrated database and just reacquires the data.
func reopenChaosDB(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	return s
}

// key30 returns a deterministic 30-char string (valid for the user_key / token
// / group_key CHECK(length=30) constraints).
func key30(seed int) string { return fmt.Sprintf("%030d", seed) }

// rid30 returns a deterministic 30-char receipt id (R + 29 digits).
func rid30(seed int) string { return fmt.Sprintf("R%029d", seed) }

func mustSeedUser(t *testing.T, s *sqlite.Store, seed int, email string) int64 {
	t.Helper()
	u := &store.User{
		Email:     email,
		PassHash:  "$argon2id$...",
		Role:      "user",
		UserKey:   key30(seed),
		QuietTZ:   "UTC",
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	id, err := s.Repos().Users.Create(context.Background(), u)
	if err != nil {
		t.Fatalf("seed user %d: %v", seed, err)
	}
	return id
}

func mustSeedApp(t *testing.T, s *sqlite.Store, userID int64, seed int) int64 {
	t.Helper()
	a := &store.App{UserID: userID, Token: key30(seed), Name: "app"}
	id, err := s.Repos().Apps.Create(context.Background(), a)
	if err != nil {
		t.Fatalf("seed app %d: %v", seed, err)
	}
	return id
}

// ingestPriority2 mirrors POST /1/messages.json for a priority-2 send: it
// atomically writes the send + per-recipient messages + a pending receipt
// (CreateFanout), then charges the sender's quota EXACTLY ONCE per recipient
// (the single charge point in messages.go). It returns the send id. This is
// the ingest half of every quota-integrity assertion.
func ingestPriority2(t *testing.T, s *sqlite.Store, senderID, appID int64, recipientIDs []int64, receiptID, tag, callbackURL string, createdAt time.Time) int64 {
	t.Helper()
	send := store.Send{
		AppID:        appID,
		SenderUserID: senderID,
		Priority:     2,
		Title:        "emergency",
		Body:         "burst",
		Tag:          tag,
		CallbackURL:  callbackURL,
		CreatedAt:    createdAt,
	}
	msgs := make([]store.Message, 0, len(recipientIDs))
	for _, rid := range recipientIDs {
		msgs = append(msgs, store.Message{RecipientUserID: rid, CreatedAt: createdAt})
	}
	receipt := &store.Receipt{ID: receiptID, State: string(receipts.StatePending), Tag: tag}
	id, err := s.Repos().Sends.CreateFanout(context.Background(),
		&store.Fanout{Send: send, Messages: msgs, Receipt: receipt})
	if err != nil {
		t.Fatalf("create fanout: %v", err)
	}
	period := quota.Period(quota.CentralTime, createdAt)
	if _, err := s.Repos().Quota.Increment(context.Background(), senderID, period, int64(len(recipientIDs))); err != nil {
		t.Fatalf("charge quota: %v", err)
	}
	return id
}

// dbRetryStore is a TEST-ONLY receipts.RetryStore backed by the real SQLite
// receipt rows (via the *sql.DB the Store exposes). It is the persistence
// adapter the production retry path uses: IncrementRetry bumps retry_count;
// SetExpired flips the state to the expired terminal. It lives here (not on
// *sqlite.ReceiptRepo) because this suite is READ-ONLY against product code,
// and the store does not yet expose these two writes as named methods (adding
// them would touch worker-owned files). The SQL mirrors the production
// conditional-UPDATE shapes used elsewhere in the driver.
type dbRetryStore struct{ db *sql.DB }

func (s *dbRetryStore) GetReceipt(ctx context.Context, id string) (receipts.ReceiptSnapshot, error) {
	var state string
	var rc int
	err := s.db.QueryRowContext(ctx,
		`SELECT state, retry_count FROM receipts WHERE id = ?`, id).Scan(&state, &rc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return receipts.ReceiptSnapshot{}, store.ErrNotFound
		}
		return receipts.ReceiptSnapshot{}, err
	}
	return receipts.ReceiptSnapshot{State: receipts.State(state), RetryCount: rc}, nil
}

func (s *dbRetryStore) IncrementRetry(ctx context.Context, id string, _ time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE receipts SET retry_count = retry_count + 1 WHERE id = ?`, id)
	return err
}

func (s *dbRetryStore) SetExpired(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE receipts SET state = 'expired', expired_at = ?
		 WHERE id = ? AND state IN ('pending','delivered')`,
		at.UTC().Format(time.RFC3339Nano), id)
	return err
}

// redeliverRecorder counts every retry redelivery. Critically its fn() touches
// NO quota -- the retry path's redelivery is a transport push, not a billable
// send. The quota-integrity test pins that invariant by asserting the counter
// is unchanged after N recorded redeliveries.
type redeliverRecorder struct {
	mu       sync.Mutex
	attempts []int
}

func (r *redeliverRecorder) fn() receipts.Redeliver {
	return func(_ context.Context, _ string, attempt int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.attempts = append(r.attempts, attempt)
		return nil
	}
}

func (r *redeliverRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attempts)
}

// fireRecorder is a concurrency-safe per-id fire counter for the timer-claim
// scenario. The exactly-once contract is: every timer id fires exactly once.
type fireRecorder struct {
	mu   sync.Mutex
	hits map[int64]int
}

func newFireRecorder() *fireRecorder { return &fireRecorder{hits: make(map[int64]int)} }

func (r *fireRecorder) handler() timers.Handler {
	return func(_ context.Context, t store.Timer) error {
		r.mu.Lock()
		r.hits[t.ID]++
		r.mu.Unlock()
		return nil
	}
}

func (r *fireRecorder) assertAllOnce(t *testing.T, want []int64) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.hits) != len(want) {
		t.Fatalf("distinct fired timers = %d, want %d; hits=%v", len(r.hits), len(want), r.hits)
	}
	for _, id := range want {
		if r.hits[id] != 1 {
			t.Fatalf("timer %d fired %d times, want exactly 1; hits=%v", id, r.hits[id], r.hits)
		}
	}
}

// countRows returns SELECT count(*) FROM table. table MUST be a literal.
func countRows(t *testing.T, s *sqlite.Store, table string) int64 {
	t.Helper()
	var n int64
	if err := s.DB().QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func countClaimed(t *testing.T, s *sqlite.Store) int64 {
	t.Helper()
	var n int64
	if err := s.DB().QueryRow(`SELECT count(*) FROM timers WHERE claimed_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count claimed: %v", err)
	}
	return n
}

func quotaCount(t *testing.T, s *sqlite.Store, userID int64, period string) int64 {
	t.Helper()
	qc, err := s.Repos().Quota.Get(context.Background(), userID, period)
	if err != nil {
		t.Fatalf("quota get: %v", err)
	}
	return qc.Count
}

func receiptState(t *testing.T, s *sqlite.Store, id string) (receipts.State, int) {
	t.Helper()
	r, err := s.Repos().Receipts.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("receipt get %s: %v", id, err)
	}
	return receipts.State(r.State), r.RetryCount
}

// seedRetryTimer enqueues the first "retry" timer for a receipt, due at fireAt,
// carrying the RetryPayload the retry handler rebuilds the scheduler from.
func seedRetryTimer(t *testing.T, eng *timers.Engine, receiptID string, createdAt, fireAt time.Time) int64 {
	t.Helper()
	payload, err := timers.MarshalPayload(timers.RetryPayload{ReceiptID: receiptID, CreatedAt: createdAt})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	id, err := eng.CreateTimer(context.Background(), &store.Timer{
		Kind: timers.KindRetry, ReceiptID: receiptID, FireAt: fireAt, Payload: payload,
	})
	if err != nil {
		t.Fatalf("seed retry timer: %v", err)
	}
	return id
}

// ---- (1) QUOTA INTEGRITY ---------------------------------------------------

// TestChaosQuotaIntegrityNoRechargeOnRetry is the todo-26 headline quota
// regression: a priority-2 (emergency) message is charged EXACTLY ONCE at
// ingest, and driving N delivery retries through the real retry scheduler +
// durable timer engine MUST NOT re-charge quota. The retry path (redeliver +
// receipt retry_count bump) never touches quota_counters; this pins that
// invariant against the explicit regression risk named in todo 10 / todo 26.
//
// MANUAL QA (failure-scenario): the raw quota count after N retries is == 1.
func TestChaosQuotaIntegrityNoRechargeOnRetry(t *testing.T) {
	t.Parallel()
	s, _ := newChaosDB(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	senderID := mustSeedUser(t, s, 1, "sender@x.io")
	recipientID := mustSeedUser(t, s, 2, "rcpt@x.io")
	appID := mustSeedApp(t, s, senderID, 3)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	period := quota.Period(quota.CentralTime, base)
	receiptID := rid30(1)

	ingestPriority2(t, s, senderID, appID, []int64{recipientID}, receiptID, "", "", base)

	// Quota charged exactly once at ingest (the single billable event).
	if got := quotaCount(t, s, senderID, period); got != 1 {
		t.Fatalf("quota after ingest = %d, want 1", got)
	}

	// Wire the REAL timer engine + retry handler over the REAL receipt row.
	clock := &chaosClock{now: base}
	redeliver := &redeliverRecorder{}
	eng := timers.NewEngine(s.TimerEngine(),
		timers.WithClock(clock.timerClock()),
		timers.WithBatch(10))
	eng.RegisterHandler(timers.KindRetry, timers.NewRetryHandler(eng,
		&dbRetryStore{db: s.DB()}, receipts.DefaultRetryPolicy(),
		clock.timerClock(), redeliver.fn(), nil))

	// First retry timer, due immediately at base (attempt 1).
	seedRetryTimer(t, eng, receiptID, base, base)

	// Drive N retries: advance the clock one interval per step and Poll once.
	// Each Poll claims the due retry timer, fires the handler (one Tick ->
	// redeliver + IncrementRetry + schedule next), and deletes the fired row.
	const N = 10
	interval := receipts.DefaultRetryInterval
	for i := 1; i <= N; i++ {
		clock.set(base.Add(time.Duration(i) * interval))
		if _, err := eng.Poll(ctx); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	// QUOTA MUST STILL BE 1 -- retries never re-charge. This is the regression
	// assertion: if a future change makes the retry path bill the sender, this
	// fails with a clear "REGRESSION" message.
	if got := quotaCount(t, s, senderID, period); got != 1 {
		t.Fatalf("REGRESSION: quota after %d retries = %d, want 1 (retries must not re-charge)", N, got)
	}
	// The receipt advanced exactly N attempts and is still live (non-terminal).
	if _, rc := receiptState(t, s, receiptID); rc != N {
		t.Fatalf("retry_count after %d retries = %d, want %d", N, rc, N)
	}
	if got := redeliver.count(); got != N {
		t.Fatalf("redeliver attempts = %d, want %d", got, N)
	}
	// Exactly ONE pending retry timer survives (the single-attempt chain).
	if got := countRows(t, s, "timers"); got != 1 {
		t.Fatalf("pending timers after %d retries = %d, want 1 (single retry chain)", N, got)
	}
}

// ---- (2a) KILL -9 during the sends-table fanout transaction ---------------

// TestChaosFanoutTransactionCrash is scenario (a): a crash DURING the
// sends-table fanout transaction leaves NO half-state (no send without its
// messages, no orphan receipt). On restart the fanout runs clean and produces
// EXACTLY ONE send -- no duplicate fanout. The mid-fanout crash is modeled by
// an abandoned (rolled-back) transaction, which is precisely what SQLite WAL
// recovery discards on a real kill -9 + restart.
func TestChaosFanoutTransactionCrash(t *testing.T) {
	t.Parallel()
	s, path := newChaosDB(t)
	ctx := context.Background()

	senderID := mustSeedUser(t, s, 1, "sender@x.io")
	rcptA := mustSeedUser(t, s, 2, "a@x.io")
	rcptB := mustSeedUser(t, s, 3, "b@x.io")
	appID := mustSeedApp(t, s, senderID, 4)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	created := base.UTC().Format(time.RFC3339Nano)

	// --- CRASH DURING fanout: begin the transaction, insert a PARTIAL fanout
	// (send + one message, no receipt), then ABANDON it -- the connection-drop
	// effect of kill -9 mid-transaction. SQLite discards uncommitted WAL
	// entries on recovery; we model that rollback directly. ---
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO sends(app_id, sender_user_id, priority, sound, title, body, url, url_title,
	html, monospace, timestamp, ttl, tag, encrypted, callback_url, receipt_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		appID, senderID, 2, "", "crash", "partial", "", "", 0, 0, 0, 0, nil, 0, nil, nil, created)
	if err != nil {
		t.Fatalf("partial insert send: %v", err)
	}
	partialSendID, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages(send_id, recipient_user_id, device_filter, delivered_at, created_at)
VALUES (?, ?, NULL, NULL, ?)`, partialSendID, rcptA, created); err != nil {
		t.Fatalf("partial insert message: %v", err)
	}
	// *** KILL -9 mid-transaction ***: the process dies before COMMIT. The
	// uncommitted transaction is rolled back (what WAL recovery does).
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback (crash model): %v", err)
	}
	_ = s.Close()

	// --- RESTART: reopen the same DB file (WAL recovery). ---
	s2 := reopenChaosDB(t, path)
	t.Cleanup(func() { _ = s2.Close() })

	// NO HALF-STATE: the abandoned fanout left zero rows of every kind.
	if got := countRows(t, s2, "sends"); got != 0 {
		t.Fatalf("after crash: sends = %d, want 0 (uncommitted tx rolled back, no half-state)", got)
	}
	if got := countRows(t, s2, "messages"); got != 0 {
		t.Fatalf("after crash: messages = %d, want 0", got)
	}
	if got := countRows(t, s2, "receipts"); got != 0 {
		t.Fatalf("after crash: receipts = %d, want 0 (no orphan receipt)", got)
	}

	// --- Restart completes the fanout: EXACTLY ONE send, no duplicates. ---
	period := quota.Period(quota.CentralTime, base)
	ingestPriority2(t, s2, senderID, appID, []int64{rcptA, rcptB}, rid30(7), "", "", base)

	if got := countRows(t, s2, "sends"); got != 1 {
		t.Fatalf("after restart fanout: sends = %d, want 1 (no duplicate fanout)", got)
	}
	if got := countRows(t, s2, "messages"); got != 2 {
		t.Fatalf("after restart fanout: messages = %d, want 2 (one per recipient)", got)
	}
	if got := countRows(t, s2, "receipts"); got != 1 {
		t.Fatalf("after restart fanout: receipts = %d, want 1", got)
	}
	// Quota charged exactly once for the completed send (2 recipients => 2).
	if got := quotaCount(t, s2, senderID, period); got != 2 {
		t.Fatalf("quota after restart = %d, want 2 (one per recipient, single charge)", got)
	}
}

// ---- (2b) KILL -9 during a retry scheduler fire ---------------------------

// TestChaosRetrySchedulerFireCrash is scenario (b): a crash DURING a retry
// fire (between claim and fire of a "retry" timer) leaves the receipt in a
// VALID state and the quota UNCHANGED. On restart the engine's Recover
// reclaims the orphaned retry timer and the retry continues from exactly
// where it left off -- no double quota charge, no duplicate retry timer, and
// the receipt is not stuck in a half-state.
func TestChaosRetrySchedulerFireCrash(t *testing.T) {
	t.Parallel()
	s, path := newChaosDB(t)
	ctx := context.Background()

	senderID := mustSeedUser(t, s, 1, "sender@x.io")
	recipientID := mustSeedUser(t, s, 2, "rcpt@x.io")
	appID := mustSeedApp(t, s, senderID, 3)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	period := quota.Period(quota.CentralTime, base)
	receiptID := rid30(1)

	ingestPriority2(t, s, senderID, appID, []int64{recipientID}, receiptID, "", "", base)
	if got := quotaCount(t, s, senderID, period); got != 1 {
		t.Fatalf("quota after ingest = %d, want 1", got)
	}

	clock := &chaosClock{now: base}
	redeliver := &redeliverRecorder{}

	// --- Engine 1: drive attempt 1, then claim attempt 2 and CRASH ---
	eng1 := timers.NewEngine(s.TimerEngine(),
		timers.WithClock(clock.timerClock()),
		timers.WithBatch(10))
	eng1.RegisterHandler(timers.KindRetry, timers.NewRetryHandler(eng1,
		&dbRetryStore{db: s.DB()}, receipts.DefaultRetryPolicy(),
		clock.timerClock(), redeliver.fn(), nil))

	seedRetryTimer(t, eng1, receiptID, base, base) // attempt 1 due at base

	// Fire attempt 1 at base.
	if _, err := eng1.Poll(ctx); err != nil {
		t.Fatalf("eng1 poll1: %v", err)
	}
	if got := redeliver.count(); got != 1 {
		t.Fatalf("redeliver after attempt1 = %d, want 1", got)
	}
	if _, rc := receiptState(t, s, receiptID); rc != 1 {
		t.Fatalf("retry_count after attempt1 = %d, want 1", rc)
	}
	// Exactly one follow-up retry timer scheduled (attempt 2 at base+30s).
	if got := countRows(t, s, "timers"); got != 1 {
		t.Fatalf("timers after attempt1 = %d, want 1 follow-up", got)
	}

	// Advance to when attempt 2 is due, CLAIM it, then *** KILL -9 *** before
	// Fire. The claimed retry timer is never fired; it survives as an orphan.
	clock.set(base.Add(receipts.DefaultRetryInterval))
	claimed, err := eng1.Claim(ctx)
	if err != nil {
		t.Fatalf("eng1 claim2: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("eng1 claim2 = %d timers, want 1", len(claimed))
	}
	_ = s.Close()

	// --- RESTART ---
	s2 := reopenChaosDB(t, path)
	t.Cleanup(func() { _ = s2.Close() })

	// Crash invariants: receipt in a VALID non-terminal state (not a corrupt
	// midpoint), retry_count == 1, quota UNCHANGED (== 1), exactly one
	// orphaned timer.
	if st, rc := receiptState(t, s2, receiptID); st != receipts.StatePending || rc != 1 {
		t.Fatalf("after crash: receipt = (%s, retry_count=%d), want (pending, 1) -- not stuck in half-state", st, rc)
	}
	if got := quotaCount(t, s2, senderID, period); got != 1 {
		t.Fatalf("REGRESSION: quota after crash = %d, want 1 (no re-charge on retry)", got)
	}
	if got := countRows(t, s2, "timers"); got != 1 {
		t.Fatalf("after crash: timers = %d, want 1 (the orphaned retry)", got)
	}
	if got := countClaimed(t, s2); got != 1 {
		t.Fatalf("after crash: orphaned claimed = %d, want 1", got)
	}

	// --- Engine 2: Recover reclaims the orphan; the retry continues. ---
	eng2 := timers.NewEngine(s2.TimerEngine(),
		timers.WithClock(clock.timerClock()),
		timers.WithBatch(10))
	eng2.RegisterHandler(timers.KindRetry, timers.NewRetryHandler(eng2,
		&dbRetryStore{db: s2.DB()}, receipts.DefaultRetryPolicy(),
		clock.timerClock(), redeliver.fn(), nil))

	if reset, err := eng2.Recover(ctx); err != nil || reset != 1 {
		t.Fatalf("eng2 recover: reset=%d err=%v, want 1/nil", reset, err)
	}
	// Fire attempt 2 (the recovered orphan).
	if _, err := eng2.Poll(ctx); err != nil {
		t.Fatalf("eng2 poll: %v", err)
	}
	if got := redeliver.count(); got != 2 {
		t.Fatalf("redeliver after restart = %d, want 2 (attempt1 + recovered attempt2)", got)
	}
	if _, rc := receiptState(t, s2, receiptID); rc != 2 {
		t.Fatalf("retry_count after restart = %d, want 2", rc)
	}
	// Quota STILL 1: the recovered retry did not re-charge.
	if got := quotaCount(t, s2, senderID, period); got != 1 {
		t.Fatalf("REGRESSION: quota after restart retry = %d, want 1", got)
	}
	// Exactly one follow-up timer (attempt 3), no duplicates, none claimed.
	if got := countRows(t, s2, "timers"); got != 1 {
		t.Fatalf("timers after restart = %d, want 1 (single follow-up, no duplicates)", got)
	}
	if got := countClaimed(t, s2); got != 0 {
		t.Fatalf("claimed after restart = %d, want 0", got)
	}
}

// ---- (2c) KILL -9 during a timer claim (happy-path MANUAL QA) -------------

// TestChaosTimerClaimCrash is scenario (c) and the happy-path MANUAL QA: N
// timers are due; the engine fires some, then is KILLED between claim and
// fire (claimed rows become orphans). On RESTART the engine's Recover resets
// the orphans and every timer fires EXACTLY ONCE against the REAL production
// store + engine -- the same crash-recovery path a real kill -9 + restart
// walks.
func TestChaosTimerClaimCrash(t *testing.T) {
	t.Parallel()
	s, path := newChaosDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	const N = 6

	// Seed N callback timers, all due at base.
	ids := make([]int64, 0, N)
	for i := 0; i < N; i++ {
		tm := store.Timer{Kind: timers.KindCallback, FireAt: base, Payload: fmt.Sprintf("c%d", i)}
		id, err := s.TimerEngine().Create(ctx, &tm)
		if err != nil {
			t.Fatalf("seed timer %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	rec := newFireRecorder()

	// --- Engine 1: fire batch1, claim batch2, then CRASH (kill -9) ---
	eng1 := timers.NewEngine(s.TimerEngine(),
		timers.WithClock(func() time.Time { return base }),
		timers.WithBatch(2))
	eng1.RegisterHandler(timers.KindCallback, rec.handler())

	batch1, err := eng1.Claim(ctx)
	if err != nil {
		t.Fatalf("eng1 claim1: %v", err)
	}
	if len(batch1) != 2 {
		t.Fatalf("batch1 = %d timers, want 2", len(batch1))
	}
	if fired, err := eng1.Fire(ctx, batch1); err != nil || fired != 2 {
		t.Fatalf("eng1 fire1: fired=%d err=%v, want 2/nil", fired, err)
	}
	batch2, err := eng1.Claim(ctx) // claimed but NOT fired
	if err != nil {
		t.Fatalf("eng1 claim2: %v", err)
	}
	if len(batch2) != 2 {
		t.Fatalf("batch2 = %d timers, want 2", len(batch2))
	}
	// *** KILL -9 ***: close the DB with batch2 claimed-but-unfired and 2
	// timers still untouched. batch2 rows survive as orphans.
	_ = batch2
	_ = s.Close()

	// --- Engine 2 (restart): Recover + drain ---
	s2 := reopenChaosDB(t, path)
	t.Cleanup(func() { _ = s2.Close() })

	// Crash invariant: 4 rows survived (2 orphaned + 2 untouched), 2 claimed.
	if got := countRows(t, s2, "timers"); got != 4 {
		t.Fatalf("after crash: rows = %d, want 4 (2 orphaned + 2 untouched)", got)
	}
	if got := countClaimed(t, s2); got != 2 {
		t.Fatalf("after crash: orphaned claimed = %d, want 2", got)
	}

	eng2 := timers.NewEngine(s2.TimerEngine(),
		timers.WithClock(func() time.Time { return base }),
		timers.WithBatch(2))
	eng2.RegisterHandler(timers.KindCallback, rec.handler())

	reset, err := eng2.Recover(ctx)
	if err != nil {
		t.Fatalf("eng2 recover: %v", err)
	}
	if reset != 2 {
		t.Fatalf("recover reset %d orphans, want 2", reset)
	}
	for i := 0; i < 4; i++ {
		if _, err := eng2.Poll(ctx); err != nil {
			t.Fatalf("eng2 poll %d: %v", i, err)
		}
	}
	if got := countRows(t, s2, "timers"); got != 0 {
		t.Fatalf("after restart drain: rows = %d, want 0 (all fired+deleted)", got)
	}
	// EXACTLY-ONCE: each of the N timers fired exactly once across both engines.
	rec.assertAllOnce(t, ids)
}

// ---- (2d) KILL -9 during ack + callback enqueue ---------------------------

// TestChaosAckCallbackEnqueueCrash is scenario (d): a crash DURING the ack +
// callback-enqueue sequence. The ack itself is atomic (one conditional
// UPDATE), so the receipt is NEVER stuck in a half-state: it is cleanly
// acknowledged. A crash between the ack commit and the callback enqueue leaves
// the receipt acknowledged with no callback row; the recovery scan
// idempotently re-enqueues the missing callback (at-least-once), so the
// webhook is delivered and the receipt is not stuck.
//
// NOTE: the production callback worker (todo 25) is not yet built; this test
// drives the recovery scan itself (recoverEnqueueCallback) to prove the
// crash-recovery INVARIANT (clean terminal receipt + idempotent at-least-once
// callback enqueue) holds. It does NOT touch product code.
func TestChaosAckCallbackEnqueueCrash(t *testing.T) {
	t.Parallel()
	s, path := newChaosDB(t)
	ctx := context.Background()

	senderID := mustSeedUser(t, s, 1, "sender@x.io")
	recipientID := mustSeedUser(t, s, 2, "rcpt@x.io")
	appID := mustSeedApp(t, s, senderID, 3)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	receiptID := rid30(1)
	const callbackURL = "https://hooks.example.com/rcpt"

	ingestPriority2(t, s, senderID, appID, []int64{recipientID}, receiptID, "", callbackURL, base)
	if st, _ := receiptState(t, s, receiptID); st != receipts.StatePending {
		t.Fatalf("pre-ack state = %s, want pending", st)
	}

	// --- ACK commits (atomic conditional UPDATE), then *** KILL -9 *** before
	// the callback enqueue (CallbackRepo.Create) runs. ---
	if err := s.Repos().Receipts.Acknowledge(ctx, receiptID, "rcpt@x.io", "dev1", base); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	_ = s.Close() // CRASH: callback enqueue never happened.

	// --- RESTART ---
	s2 := reopenChaosDB(t, path)
	t.Cleanup(func() { _ = s2.Close() })

	// The receipt is CLEANLY acknowledged (atomic ack) -- not stuck in a
	// half-state like a corrupt midpoint or "acknowledged-but-pending".
	if st, _ := receiptState(t, s2, receiptID); st != receipts.StateAcknowledged {
		t.Fatalf("after crash: receipt state = %s, want acknowledged (atomic ack, no half-state)", st)
	}
	// The callback enqueue did NOT happen before the crash.
	if got := countRows(t, s2, "callbacks"); got != 0 {
		t.Fatalf("after crash: callbacks = %d, want 0 (enqueue interrupted)", got)
	}

	// --- RECOVERY: idempotent re-enqueue of the missing callback. ---
	if !recoverEnqueueCallback(t, s2, receiptID) {
		t.Fatalf("recover enqueue #1: expected to enqueue a callback, returned false")
	}
	// A second recovery pass MUST be idempotent (no duplicate callback).
	if recoverEnqueueCallback(t, s2, receiptID) {
		t.Fatalf("recover enqueue #2: idempotent scan enqueued a DUPLICATE callback")
	}
	if got := countRows(t, s2, "callbacks"); got != 1 {
		t.Fatalf("after recovery: callbacks = %d, want 1 (idempotent re-enqueue, at-least-once)", got)
	}
	// The receipt is still cleanly acknowledged (recovery did not un-ack).
	if st, _ := receiptState(t, s2, receiptID); st != receipts.StateAcknowledged {
		t.Fatalf("after recovery: receipt state = %s, want acknowledged", st)
	}
}

// recoverEnqueueCallback is the TEST-ONLY recovery scan that the production
// callback worker (todo 25) would run: for an acknowledged receipt whose
// parent send carries a callback_url but has no callback row, create exactly
// one callback row. The INSERT...WHERE NOT EXISTS makes it atomic and
// idempotent, so a repeated scan (or a scan racing another) never duplicates
// the row -- the at-least-once guarantee without a duplicate.
func recoverEnqueueCallback(t *testing.T, s *sqlite.Store, receiptID string) (enqueued bool) {
	t.Helper()
	ctx := context.Background()
	var cbURL sql.NullString
	err := s.DB().QueryRowContext(ctx,
		`SELECT s.callback_url FROM receipts r JOIN sends s ON r.send_id = s.id WHERE r.id = ?`,
		receiptID).Scan(&cbURL)
	if err != nil {
		t.Fatalf("recover enqueue: read callback_url: %v", err)
	}
	if !cbURL.Valid || cbURL.String == "" {
		return false // nothing to enqueue
	}
	res, err := s.DB().ExecContext(ctx,
		`INSERT INTO callbacks(receipt_id, url, state, next_attempt_at, attempts)
		 SELECT ?, ?, 'pending', NULL, 0
		 WHERE NOT EXISTS (SELECT 1 FROM callbacks WHERE receipt_id = ?)`,
		receiptID, cbURL.String, receiptID)
	if err != nil {
		t.Fatalf("recover enqueue insert: %v", err)
	}
	n, _ := res.RowsAffected()
	return n > 0
}
