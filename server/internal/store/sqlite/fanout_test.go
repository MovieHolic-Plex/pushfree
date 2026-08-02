package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// TestFanoutOneTransaction verifies the happy path: one send + three messages
// + one receipt all commit atomically, and every inserted row is readable with
// correct cross-references (messages.send_id == send.id, receipt.send_id ==
// send.id).
func TestFanoutOneTransaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	sender := mustSeedUser(t, s.users, 100, "sender@example.com")
	r1 := mustSeedUser(t, s.users, 101, "r1@example.com")
	r2 := mustSeedUser(t, s.users, 102, "r2@example.com")
	r3 := mustSeedUser(t, s.users, 103, "r3@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 200)

	now := time.Now()
	receipt := &store.Receipt{ID: key30(300), State: "pending", ExpiresAt: ptrTime(now.Add(time.Hour))}
	sendID, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send: store.Send{
			AppID: app.ID, SenderUserID: sender.ID, Priority: 2,
			Title: "t", Body: "b", HTML: true, Timestamp: now.Unix(), Tag: "ops",
			CallbackURL: "https://example.com/cb", CreatedAt: now,
		},
		Messages: []store.Message{
			{RecipientUserID: r1.ID, CreatedAt: now},
			{RecipientUserID: r2.ID, DeviceFilter: "phone", CreatedAt: now},
			{RecipientUserID: r3.ID, CreatedAt: now},
		},
		Receipt: receipt,
	})
	if err != nil {
		t.Fatalf("CreateFanout: %v", err)
	}

	// The send carries the priority-2 receipt_id back-reference.
	got, err := s.sends.GetByID(ctx, sendID)
	if err != nil {
		t.Fatalf("GetByID send: %v", err)
	}
	if got.ReceiptID != key30(300) || !got.HTML || got.Tag != "ops" || got.Priority != 2 {
		t.Fatalf("send round-trip mismatch: %+v", got)
	}

	// Three messages, each pointing at this send.
	for i, uid := range []int64{r1.ID, r2.ID, r3.ID} {
		msgs, err := s.msgs.ListSince(ctx, uid, 0, 10)
		if err != nil {
			t.Fatalf("ListSince %d: %v", i, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("recipient %d: got %d messages, want 1", i, len(msgs))
		}
		if msgs[0].SendID != sendID {
			t.Fatalf("recipient %d: send_id = %d, want %d", i, msgs[0].SendID, sendID)
		}
	}
	// r2 keeps its device_filter.
	r2Msgs, _ := s.msgs.ListSince(ctx, r2.ID, 0, 10)
	if r2Msgs[0].DeviceFilter != "phone" {
		t.Fatalf("device_filter lost: %+v", r2Msgs[0])
	}

	// Receipt links back to the send.
	rc, err := s.rcpts.GetByID(ctx, key30(300))
	if err != nil {
		t.Fatalf("GetByID receipt: %v", err)
	}
	if rc.SendID != sendID || rc.State != "pending" {
		t.Fatalf("receipt mismatch: %+v", rc)
	}
}

// TestFanoutRollbackOnFailure proves the whole fan-out rolls back if any step
// fails. A receipt with an invalid state trips the CHECK(state IN (...))
// constraint AFTER the send and messages are inserted; the rollback must
// leave zero sends, zero messages, and zero receipts for that attempt.
func TestFanoutRollbackOnFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	sender := mustSeedUser(t, s.users, 110, "sender2@example.com")
	recipient := mustSeedUser(t, s.users, 111, "recv2@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 210)

	now := time.Now()
	// State "bogus" violates CHECK(state IN (...)) -> receipt insert fails.
	badReceipt := &store.Receipt{ID: key30(310), State: "bogus"}
	_, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send:     store.Send{AppID: app.ID, SenderUserID: sender.ID, Body: "x", CreatedAt: now},
		Messages: []store.Message{{RecipientUserID: recipient.ID, CreatedAt: now}},
		Receipt:  badReceipt,
	})
	if err == nil {
		t.Fatal("CreateFanout with bad receipt: want error, got nil")
	}

	// No send row should exist for this attempt.
	sends, err := s.db.Query(`SELECT count(*) FROM sends`)
	if err != nil {
		t.Fatalf("count sends: %v", err)
	}
	defer sends.Close()
	if !sends.Next() {
		t.Fatal("no rows from sends count")
	}
	var sendCount int
	if err := sends.Scan(&sendCount); err != nil {
		t.Fatalf("scan sends: %v", err)
	}
	sends.Close()
	if sendCount != 0 {
		t.Fatalf("rollback failed: sends count = %d, want 0", sendCount)
	}

	// No messages, no receipts either.
	for _, q := range []string{
		`SELECT count(*) FROM messages`,
		`SELECT count(*) FROM receipts`,
	} {
		var n int
		if err := s.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		if n != 0 {
			t.Fatalf("rollback failed: %q count = %d, want 0", q, n)
		}
	}
}

// TestUniqueViolations is table-driven over every UNIQUE column: a second
// insert with a duplicated key must fail with the unique-violation sentinel.
// The raw driver error text is logged to help future debugging.
func TestUniqueViolations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("user_key", func(t *testing.T) {
		s := newDB(t)
		mustSeedUser(t, s.users, 200, "u1@example.com")
		dup := store.User{Email: "u2@example.com", PassHash: "h", Role: "user", UserKey: key30(200), CreatedAt: time.Now()}
		_, err := s.users.Create(ctx, &dup)
		if !store.IsUniqueViolation(err) {
			t.Fatalf("dup user_key: err = %v, want unique violation", err)
		}
		t.Logf("duplicate user_key raw error: %v", err)
	})

	t.Run("email", func(t *testing.T) {
		s := newDB(t)
		mustSeedUser(t, s.users, 201, "shared@example.com")
		dup := store.User{Email: "shared@example.com", PassHash: "h", Role: "user", UserKey: key30(777), CreatedAt: time.Now()}
		_, err := s.users.Create(ctx, &dup)
		if !store.IsUniqueViolation(err) {
			t.Fatalf("dup email: err = %v, want unique violation", err)
		}
		t.Logf("duplicate email raw error: %v", err)
	})

	t.Run("token", func(t *testing.T) {
		s := newDB(t)
		u := mustSeedUser(t, s.users, 202, "t1@example.com")
		mustSeedApp(t, s.apps, u.ID, 300) // token = key30(300)
		dup := store.App{UserID: u.ID, Token: key30(300), Name: "dup"}
		_, err := s.apps.Create(ctx, &dup)
		if !store.IsUniqueViolation(err) {
			t.Fatalf("dup token: err = %v, want unique violation", err)
		}
		t.Logf("duplicate token raw error: %v", err)
	})

	t.Run("device_id", func(t *testing.T) {
		s := newDB(t)
		u := mustSeedUser(t, s.users, 203, "dev1@example.com")
		first := store.Device{UserID: u.ID, DeviceID: "device-X", SecretHash: "h", Name: "phone"}
		if _, err := s.devs.Create(ctx, &first); err != nil {
			t.Fatalf("create first device: %v", err)
		}
		dup := store.Device{UserID: u.ID, DeviceID: "device-X", SecretHash: "h2", Name: "tablet"}
		_, err := s.devs.Create(ctx, &dup)
		if !store.IsUniqueViolation(err) {
			t.Fatalf("dup device_id: err = %v, want unique violation", err)
		}
		t.Logf("duplicate device_id raw error: %v", err)
	})

	// A non-unique error must NOT be classified as a unique violation, so
	// the sentinel is not over-eager.
	if store.IsUniqueViolation(errors.New("some other error")) {
		t.Fatal("IsUniqueViolation misclassified an unrelated error")
	}
}

// TestReceiptSendOneToOne enforces the receipt<->send 1:1: a second receipt
// for the same send_id must fail with a unique violation
// (UNIQUE(receipts.send_id)).
func TestReceiptSendOneToOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	sender := mustSeedUser(t, s.users, 120, "one2one@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 220)

	now := time.Now()
	// First fanout creates send S + receipt R1.
	r1 := &store.Receipt{ID: key30(400), State: "pending"}
	sendID, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send:     store.Send{AppID: app.ID, SenderUserID: sender.ID, Body: "first", CreatedAt: now},
		Receipt:  r1,
	})
	if err != nil {
		t.Fatalf("first fanout: %v", err)
	}

	// A second receipt for the SAME send_id must fail (1:1 enforced).
	r2 := &store.Receipt{ID: key30(401), State: "pending", SendID: sendID}
	err = s.rcpts.Create(ctx, r2)
	if !store.IsUniqueViolation(err) {
		t.Fatalf("second receipt same send_id: err = %v, want unique violation", err)
	}
	t.Logf("second receipt same send_id raw error: %v", err)
}

// TestMessagesListSinceOrdering inserts several messages for one recipient
// interleaved with another recipient, then asserts ListSince returns only
// that recipient's rows in ascending id order with the afterID cursor.
func TestMessagesListSinceOrdering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	sender := mustSeedUser(t, s.users, 130, "lst@example.com")
	target := mustSeedUser(t, s.users, 131, "tgt@example.com")
	other := mustSeedUser(t, s.users, 132, "oth@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 230)

	// Helper to fan out one send to a single recipient and return its msg id.
	sendOne := func(recipient int64) int64 {
		t.Helper()
		now := time.Now()
		sid, err := s.sends.CreateFanout(ctx, &store.Fanout{
			Send:     store.Send{AppID: app.ID, SenderUserID: sender.ID, Body: "x", CreatedAt: now},
			Messages: []store.Message{{RecipientUserID: recipient, CreatedAt: now}},
		})
		if err != nil {
			t.Fatalf("sendOne: %v", err)
		}
		msgs, err := s.msgs.ListSince(ctx, recipient, 0, 10)
		if err != nil {
			t.Fatalf("sendOne list: %v", err)
		}
		// Return the just-inserted message id (the highest for this recipient).
		var last int64
		for _, m := range msgs {
			if m.SendID == sid {
				last = m.ID
			}
		}
		return last
	}

	// target, other, target, other, target -> target owns ids 1,3,5
	id1 := sendOne(target.ID)
	sendOne(other.ID)
	id3 := sendOne(target.ID)
	sendOne(other.ID)
	id5 := sendOne(target.ID)
	if !(id1 < id3 && id3 < id5) {
		t.Fatalf("ids not strictly increasing: %d %d %d", id1, id3, id5)
	}

	// Full list for target in ascending order.
	all, err := s.msgs.ListSince(ctx, target.ID, 0, 100)
	if err != nil {
		t.Fatalf("ListSince all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("target list len = %d, want 3", len(all))
	}
	if all[0].ID != id1 || all[1].ID != id3 || all[2].ID != id5 {
		t.Fatalf("ordering wrong: got ids %d %d %d, want %d %d %d",
			all[0].ID, all[1].ID, all[2].ID, id1, id3, id5)
	}

	// afterID cursor: only rows after id3.
	tail, err := s.msgs.ListSince(ctx, target.ID, id3, 100)
	if err != nil {
		t.Fatalf("ListSince tail: %v", err)
	}
	if len(tail) != 1 || tail[0].ID != id5 {
		t.Fatalf("cursor tail wrong: %+v", tail)
	}

	// limit is honored.
	limited, err := s.msgs.ListSince(ctx, target.ID, 0, 2)
	if err != nil {
		t.Fatalf("ListSince limited: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit not honored: len = %d, want 2", len(limited))
	}
}

// TestTimersClaimDueOnce proves each due timer is claimed exactly once when
// two goroutines poll concurrently against one database (busy_timeout
// serializes the writers). The race detector must stay clean.
func TestTimersClaimDueOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	const due = 500
	now := time.Now()
	// Seed `due` timers all already due (fire_at in the past).
	for i := 0; i < due; i++ {
		if _, err := s.timrs.Create(ctx, &store.Timer{
			Kind:   "retry",
			FireAt: now.Add(-time.Hour),
			Payload: "p",
		}); err != nil {
			t.Fatalf("seed timer %d: %v", i, err)
		}
	}
	// A future timer that must NOT be claimed.
	if _, err := s.timrs.Create(ctx, &store.Timer{
		Kind: "expire", FireAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("seed future timer: %v", err)
	}

	var (
		mu         sync.Mutex
		claimedIDs = make(map[int64]int)
		anyErr     error
		wg         sync.WaitGroup
	)
	// Two workers drain the due set in batches.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				batch, err := s.timrs.ClaimDue(ctx, now, 25)
				if err != nil {
					mu.Lock()
					anyErr = err
					mu.Unlock()
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, tm := range batch {
					claimedIDs[tm.ID]++
					if tm.ClaimedAt == nil {
						mu.Unlock()
						t.Errorf("claimed timer %d has nil ClaimedAt", tm.ID)
						mu.Lock()
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if anyErr != nil {
		t.Fatalf("ClaimDue error: %v", anyErr)
	}
	if len(claimedIDs) != due {
		t.Fatalf("claimed %d unique timers, want %d", len(claimedIDs), due)
	}
	// Each timer claimed exactly once.
	for id, n := range claimedIDs {
		if n != 1 {
			t.Fatalf("timer %d claimed %d times, want exactly 1", id, n)
		}
	}
	// The future timer must remain unclaimed: a subsequent claim returns
	// nothing.
	leftover, err := s.timrs.ClaimDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("leftover ClaimDue: %v", err)
	}
	if len(leftover) != 0 {
		t.Fatalf("future timer was claimed: %+v", leftover)
	}
}
