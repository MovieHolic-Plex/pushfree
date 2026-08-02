package sqlite

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// countRows is a raw count helper so tests assert against the actual table
// state rather than a repo round-trip (the misleading_success_output guard:
// we look at the database directly).
func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// seedSendAt inserts one send (no receipt) created at the given time with the
// given ttl, plus one undelivered message to recipient. It returns the send id.
func seedSendAt(t *testing.T, s *Store, appID, senderID, recipientID int64, createdAt time.Time, ttl int64) int64 {
	t.Helper()
	sid, err := s.sends.CreateFanout(context.Background(), &store.Fanout{
		Send: store.Send{
			AppID:        appID,
			SenderUserID: senderID,
			Body:         "x",
			TTL:          ttl,
			CreatedAt:    createdAt,
		},
		Messages: []store.Message{{RecipientUserID: recipientID, CreatedAt: createdAt}},
	})
	if err != nil {
		t.Fatalf("seedSendAt: %v", err)
	}
	return sid
}

// TestRetentionMessagesDelete verifies the 30-day message retention sweep:
// a 31-day-old message is deleted while a recent one survives. Asserted with
// raw before/after counts and a ListSince round-trip.
func TestRetentionMessagesDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	sender := mustSeedUser(t, s.users, 610, "ret-sender@example.com")
	recipient := mustSeedUser(t, s.users, 611, "ret-recv@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 612)

	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	old := now.Add(-31 * 24 * time.Hour) // 31 days ago -> past 30d retention

	// Old send+message (31d) and recent send+message (now).
	oldSend := seedSendAt(t, s, app.ID, sender.ID, recipient.ID, old, 0)
	_ = seedSendAt(t, s, app.ID, sender.ID, recipient.ID, now, 0)

	if before := countRows(t, s, `SELECT count(*) FROM messages`); before != 2 {
		t.Fatalf("messages before sweep = %d, want 2", before)
	}
	// The recipient saw two fan-out rows before the sweep.
	beforeList, err := s.msgs.ListSince(ctx, recipient.ID, 0, 100)
	if err != nil {
		t.Fatalf("ListSince before: %v", err)
	}
	if len(beforeList) != 2 {
		t.Fatalf("ListSince before = %d msgs, want 2", len(beforeList))
	}

	cutoff := now.Add(-30 * 24 * time.Hour) // 30-day retention cutoff
	n, err := s.DeleteMessagesBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteMessagesBefore: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteMessagesBefore deleted %d, want 1", n)
	}

	if after := countRows(t, s, `SELECT count(*) FROM messages`); after != 1 {
		t.Fatalf("messages after sweep = %d, want 1", after)
	}
	// The surviving message belongs to the recent send.
	afterList, err := s.msgs.ListSince(ctx, recipient.ID, 0, 100)
	if err != nil {
		t.Fatalf("ListSince after: %v", err)
	}
	if len(afterList) != 1 || afterList[0].SendID == oldSend {
		t.Fatalf("surviving message wrong: %+v", afterList)
	}

	// Idempotent re-run: nothing more to delete (stale_state / double-run safe).
	n2, err := s.DeleteMessagesBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteMessagesBefore rerun: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("DeleteMessagesBefore rerun deleted %d, want 0", n2)
	}
}

// TestRetentionAttachmentBlobClearedRowKept verifies the 3-day undownloaded
// attachment rule: an undownloaded BLOB on a >3d-old send is zeroed, but the
// row (and its content-type) survives; downloaded attachments and newer
// undownloaded attachments are untouched.
func TestRetentionAttachmentBlobClearedRowKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	sender := mustSeedUser(t, s.users, 620, "att-sender@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 621)

	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	old := now.Add(-4 * 24 * time.Hour) // 4 days ago -> past 3d attachment rule

	// Old send with an UNDOWNLOADED attachment (should be cleared, row kept).
	oldUndownloadedSend := seedSendAt(t, s, app.ID, sender.ID, sender.ID, old, 0)
	if _, err := s.atts.Create(ctx, &store.Attachment{
		SendID: oldUndownloadedSend, ContentType: "image/png", Data: []byte("PNG-BYTES"),
	}); err != nil {
		t.Fatalf("create old undownloaded att: %v", err)
	}
	// Old send with a DOWNLOADED attachment (should be untouched).
	oldDownloadedSend := seedSendAt(t, s, app.ID, sender.ID, sender.ID, old, 0)
	dl := old.Add(time.Hour)
	if _, err := s.atts.Create(ctx, &store.Attachment{
		SendID: oldDownloadedSend, ContentType: "image/jpeg", Data: []byte("JPEG-BYTES"), DownloadedAt: &dl,
	}); err != nil {
		t.Fatalf("create old downloaded att: %v", err)
	}
	// New send with an UNDOWNLOADED attachment (should be untouched: too new).
	newSend := seedSendAt(t, s, app.ID, sender.ID, sender.ID, now, 0)
	if _, err := s.atts.Create(ctx, &store.Attachment{
		SendID: newSend, ContentType: "image/gif", Data: []byte("GIF-BYTES"),
	}); err != nil {
		t.Fatalf("create new undownloaded att: %v", err)
	}

	before := countRows(t, s, `SELECT count(*) FROM attachments`)
	if before != 3 {
		t.Fatalf("attachments before = %d, want 3", before)
	}

	cutoff := now.Add(-3 * 24 * time.Hour) // 3-day cutoff
	n, err := s.ClearUndownloadedAttachmentBLOBs(ctx, cutoff)
	if err != nil {
		t.Fatalf("ClearUndownloadedAttachmentBLOBs: %v", err)
	}
	if n != 1 {
		t.Fatalf("cleared %d BLOBs, want 1 (only old undownloaded)", n)
	}

	// Row count unchanged: the row is KEPT.
	if after := countRows(t, s, `SELECT count(*) FROM attachments`); after != 3 {
		t.Fatalf("attachments after = %d, want 3 (rows must be kept)", after)
	}

	// The old undownloaded BLOB is gone but the row + content-type remain.
	var data []byte
	var ct string
	var dlNS int
	if err := s.db.QueryRow(
		`SELECT data, content_type, downloaded_at IS NULL FROM attachments WHERE send_id = ?`,
		oldUndownloadedSend).Scan(&data, &ct, &dlNS); err != nil {
		t.Fatalf("select cleared att: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("old undownloaded BLOB not cleared: len=%d", len(data))
	}
	if ct != "image/png" {
		t.Fatalf("content-type lost: %q", ct)
	}
	if dlNS != 1 {
		t.Fatalf("downloaded_at should still be NULL, got present")
	}

	// Downloaded attachment untouched.
	got, err := s.atts.GetBySendID(ctx, oldDownloadedSend)
	if err != nil {
		t.Fatalf("GetBySendID downloaded: %v", err)
	}
	if string(got.Data) != "JPEG-BYTES" {
		t.Fatalf("downloaded BLOB altered: %q", string(got.Data))
	}
	// New undownloaded attachment untouched.
	gotNew, err := s.atts.GetBySendID(ctx, newSend)
	if err != nil {
		t.Fatalf("GetBySendID new: %v", err)
	}
	if string(gotNew.Data) != "GIF-BYTES" {
		t.Fatalf("new undownloaded BLOB altered: %q", string(gotNew.Data))
	}

	// Idempotent: re-running clears nothing more (length(data) > 0 guard).
	n2, err := s.ClearUndownloadedAttachmentBLOBs(ctx, cutoff)
	if err != nil {
		t.Fatalf("rerun clear: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("rerun cleared %d, want 0", n2)
	}
}

// TestTtlExpiryDiscardsUndelivered verifies the TTL discard rule: when a
// send's TTL has elapsed, only its UNDELIVERED messages are discarded;
// delivered messages survive, and ttl=0 (never expires) sends are untouched.
func TestTtlExpiryDiscardsUndelivered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	sender := mustSeedUser(t, s.users, 630, "ttl-sender@example.com")
	recipient := mustSeedUser(t, s.users, 631, "ttl-recv@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 632)

	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	elapsed := now.Add(-2 * time.Minute) // created 2 minutes ago

	// Send with ttl=60s: deadline = elapsed + 60s = now-60s, which is past now.
	// One delivered message (kept) and one undelivered (discarded).
	expiredSend, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send: store.Send{
			AppID: app.ID, SenderUserID: sender.ID, Body: "ttl", TTL: 60, CreatedAt: elapsed,
		},
		Messages: []store.Message{
			{RecipientUserID: recipient.ID, DeliveredAt: ptrTime(elapsed.Add(30 * time.Second)), CreatedAt: elapsed},
			{RecipientUserID: recipient.ID, CreatedAt: elapsed}, // undelivered -> discarded
		},
	})
	if err != nil {
		t.Fatalf("create expired send: %v", err)
	}
	// ttl=0 send: never expires, undelivered message must survive.
	_ = seedSendAt(t, s, app.ID, sender.ID, recipient.ID, elapsed, 0)
	// ttl=60s send created NOW: not yet elapsed, undelivered survives.
	_ = seedSendAt(t, s, app.ID, sender.ID, recipient.ID, now, 60)

	before := countRows(t, s, `SELECT count(*) FROM messages`)
	if before != 4 {
		t.Fatalf("messages before = %d, want 4", before)
	}

	n, err := s.DeleteUndeliveredExpiredByTTL(ctx, now)
	if err != nil {
		t.Fatalf("DeleteUndeliveredExpiredByTTL: %v", err)
	}
	if n != 1 {
		t.Fatalf("discarded %d, want 1", n)
	}

	// Exactly one row (the undelivered message of the expired TTL send) gone.
	if after := countRows(t, s, `SELECT count(*) FROM messages`); after != 3 {
		t.Fatalf("messages after = %d, want 3", after)
	}
	// The delivered message of the expired send survives.
	var deliveredSurvives int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM messages WHERE send_id = ? AND delivered_at IS NOT NULL`,
		expiredSend).Scan(&deliveredSurvives); err != nil {
		t.Fatalf("count delivered of expired: %v", err)
	}
	if deliveredSurvives != 1 {
		t.Fatalf("delivered message of expired send = %d, want 1 (delivered kept)", deliveredSurvives)
	}
	// No undelivered messages remain for the expired send.
	var undeliveredLeft int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM messages WHERE send_id = ? AND delivered_at IS NULL`,
		expiredSend).Scan(&undeliveredLeft); err != nil {
		t.Fatalf("count undelivered of expired: %v", err)
	}
	if undeliveredLeft != 0 {
		t.Fatalf("undelivered of expired send = %d, want 0 (all discarded)", undeliveredLeft)
	}

	// Idempotent: re-running discards nothing more.
	n2, err := s.DeleteUndeliveredExpiredByTTL(ctx, now)
	if err != nil {
		t.Fatalf("rerun ttl: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("rerun discarded %d, want 0", n2)
	}
}

// TestRetentionSweepUnderContention verifies busy-lock robustness: while
// several goroutines hammer the store with writes, the retention deletes
// complete without surfacing SQLITE_BUSY. busy_timeout=5000 serializes the
// writers; this test asserts that contract rather than any timing.
func TestRetentionSweepUnderContention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	sender := mustSeedUser(t, s.users, 640, "lock-sender@example.com")
	recipient := mustSeedUser(t, s.users, 641, "lock-recv@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 642)

	// Pre-seed an old message so the sweep has something to delete mid-storm.
	now := time.Now()
	seedSendAt(t, s, app.ID, sender.ID, recipient.ID, now.Add(-31*24*time.Hour), 0)

	const writers = 8
	const perWriter = 25
	var wg sync.WaitGroup
	var writeErr error
	var errMu sync.Mutex
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				_, err := s.sends.CreateFanout(ctx, &store.Fanout{
					Send: store.Send{
						AppID: app.ID, SenderUserID: sender.ID, Body: "storm", CreatedAt: now,
					},
					Messages: []store.Message{{RecipientUserID: recipient.ID, CreatedAt: now}},
				})
				if err != nil {
					errMu.Lock()
					writeErr = err
					errMu.Unlock()
					return
				}
			}
		}()
	}

	// Run the sweep while writers contend. It must not return a busy/locked
	// error; busy_timeout absorbs the contention.
	cutoff := now.Add(-30 * 24 * time.Hour)
	_, sweepErr := s.DeleteMessagesBefore(ctx, cutoff)
	wg.Wait()

	if writeErr != nil {
		t.Fatalf("concurrent write failed: %v", writeErr)
	}
	if sweepErr != nil {
		low := strings.ToLower(sweepErr.Error())
		if strings.Contains(low, "busy") || strings.Contains(low, "locked") {
			t.Fatalf("sweep surfaced a busy-lock error under contention: %v", sweepErr)
		}
		t.Fatalf("sweep returned unexpected error: %v", sweepErr)
	}
}

// TestWALCheckpointRuns confirms WALCheckpoint returns a sane three-value
// result on a real temp database (it is the shutdown evidence path).
func TestWALCheckpointRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	// Force at least one WAL frame so the checkpoint has something to do.
	sender := mustSeedUser(t, s.users, 650, "wal-sender@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 651)
	seedSendAt(t, s, app.ID, sender.ID, sender.ID, time.Now(), 0)

	r, err := s.WALCheckpoint(ctx)
	if err != nil {
		t.Fatalf("WALCheckpoint: %v", err)
	}
	// busy=0 means the checkpoint ran cleanly (no active reader/writer
	// blocked it). We do not assert on Log/Checkpointed magnitudes since
	// they depend on timing of WAL flushing.
	if r.Busy != 0 {
		t.Fatalf("wal_checkpoint busy=%d, want 0 (clean)", r.Busy)
	}
}
