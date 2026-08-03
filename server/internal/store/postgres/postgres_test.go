package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/pushfree/pushfree/internal/store"
)

// This test suite exercises the Postgres implementation of store.Repos behind
// a real Postgres instance. It has TWO run modes:
//
//  1. PUSHFREE_TEST_PG_DSN set -> connect to that DSN directly. This is the CI
//     path: the GitHub Actions services container supplies the DSN, so no
//     Docker is needed inside the job.
//  2. PUSHFREE_TEST_PG=1 (no DSN) -> spin up a Postgres container via
//     testcontainers-go. This is the local-dev verification path and requires
//     a running Docker daemon.
//
// When NEITHER is set the whole suite is skipped: this box has no local
// Postgres and we never fake a pass. The skip is the acceptance signal for
// the "SKIPPED gracefully when PUSHFREE_TEST_PG unset" requirement.

// testDSN returns a live Postgres DSN for the suite. It skips the suite when
// no opt-in env var is present, and (for the local path) starts a
// testcontainers Postgres container that is torn down at suite end.
func testDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("PUSHFREE_TEST_PG_DSN"); dsn != "" {
		return dsn
	}
	if os.Getenv("PUSHFREE_TEST_PG") == "" {
		t.Skip("set PUSHFREE_TEST_PG=1 (or PUSHFREE_TEST_PG_DSN=postgres://...) to run postgres tests")
	}
	ctx := context.Background()
	pgC, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("pushfree_test"),
		postgres.WithUsername("pushfree"),
		postgres.WithPassword("pushfree"),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := pgC.Terminate(ctx); err != nil {
			t.Errorf("terminate container: %v", err)
		}
	})
	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("container connection string: %v", err)
	}
	return dsn
}

// freshStore opens a fresh, migrated store against the suite DSN, resetting
// the public schema first so subtests are isolated. The schema reset drops
// schema_migrations too, so Up re-runs every migration each time.
func freshStore(t *testing.T, dsn string) *Store {
	t.Helper()
	db, err := OpenRaw(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := Up(context.Background(), db); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return NewStore(db)
}

func TestPostgresSuite(t *testing.T) {
	dsn := testDSN(t)

	t.Run("Migrations_Idempotent_VersionPinned", func(t *testing.T) {
		s := freshStore(t, dsn)
		db := s.DB()
		// Running Up again must be a no-op (idempotent).
		if err := Up(context.Background(), db); err != nil {
			t.Fatalf("second Up: %v", err)
		}
		v, err := Version(context.Background(), db)
		if err != nil {
			t.Fatalf("version: %v", err)
		}
		if v != 4 {
			t.Fatalf("version = %d, want 4", v)
		}
		var n int
		if err := db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM schema_migrations WHERE dirty = FALSE`).Scan(&n); err != nil {
			t.Fatalf("count migrations: %v", err)
		}
		if n != 4 {
			t.Fatalf("applied migrations = %d, want 4", n)
		}
	})

	t.Run("Users_BootstrapAndQuietHours", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

		first := &store.User{Email: "a@example.com", PassHash: "h", UserKey: keyN("a", 0), CreatedAt: now}
		id1, err := s.users.CreateBootstrap(ctx, first)
		if err != nil {
			t.Fatalf("create first: %v", err)
		}
		if first.Role != "admin" {
			t.Fatalf("first user role = %q, want admin", first.Role)
		}
		second := &store.User{Email: "b@example.com", PassHash: "h", UserKey: keyN("a", 1), CreatedAt: now}
		if _, err := s.users.CreateBootstrap(ctx, second); err != nil {
			t.Fatalf("create second: %v", err)
		}
		if second.Role != "user" {
			t.Fatalf("second user role = %q, want user", second.Role)
		}
		// Lookups.
		if got, err := s.users.GetByID(ctx, id1); err != nil || got.Email != "a@example.com" {
			t.Fatalf("GetByID: got=%+v err=%v", got, err)
		}
		if _, err := s.users.GetByEmail(ctx, "a@example.com"); err != nil {
			t.Fatalf("GetByEmail: %v", err)
		}
		if _, err := s.users.GetByUserKey(ctx, first.UserKey); err != nil {
			t.Fatalf("GetByUserKey: %v", err)
		}
		// Duplicate email -> unique violation.
		dup := &store.User{Email: "a@example.com", PassHash: "h", UserKey: keyN("a", 2), CreatedAt: now}
		if _, err := s.users.CreateBootstrap(ctx, dup); !store.IsUniqueViolation(err) {
			t.Fatalf("dup email err = %v, want unique violation", err)
		}
		// Quiet hours round-trip, with clear.
		if err := s.users.UpdateQuietHours(ctx, id1, "22:00", "07:00", "UTC"); err != nil {
			t.Fatalf("update quiet: %v", err)
		}
		got, _ := s.users.GetByID(ctx, id1)
		if got.QuietStart != "22:00" || got.QuietEnd != "07:00" {
			t.Fatalf("quiet = %q-%q, want 22:00-07:00", got.QuietStart, got.QuietEnd)
		}
		if err := s.users.UpdateQuietHours(ctx, id1, "", "", "UTC"); err != nil {
			t.Fatalf("clear quiet: %v", err)
		}
		got, _ = s.users.GetByID(ctx, id1)
		if got.QuietStart != "" || got.QuietEnd != "" {
			t.Fatalf("cleared quiet = %q-%q, want empty", got.QuietStart, got.QuietEnd)
		}
		if err := s.users.UpdateQuietHours(ctx, 99999, "", "", "UTC"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing quiet update err = %v, want ErrNotFound", err)
		}
	})

	t.Run("Apps_CRUD", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		u := mustUser(t, s, "app@example.com", 0)
		a := &store.App{UserID: u.ID, Token: keyN("t", 0), Name: "n"}
		id, err := s.apps.Create(ctx, a)
		if err != nil || id == 0 {
			t.Fatalf("create app: %v id=%d", err, id)
		}
		if got, err := s.apps.GetByToken(ctx, a.Token); err != nil || got.ID != id {
			t.Fatalf("get by token: %+v %v", got, err)
		}
		apps, err := s.apps.ListByUser(ctx, u.ID)
		if err != nil || len(apps) != 1 {
			t.Fatalf("list: %d %v", len(apps), err)
		}
		// Cross-user delete is not-found (no enumeration).
		if err := s.apps.DeleteByToken(ctx, u.ID+1, a.Token); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cross-user delete err = %v, want ErrNotFound", err)
		}
		if err := s.apps.DeleteByToken(ctx, u.ID, a.Token); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.apps.GetByToken(ctx, a.Token); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("after delete get err = %v, want ErrNotFound", err)
		}
	})

	t.Run("Devices_AndFCMClear", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		u := mustUser(t, s, "dev@example.com", 0)
		d := &store.Device{UserID: u.ID, DeviceID: "dev1", SecretHash: "sh", Name: "phone", FCMToken: "tok"}
		if _, err := s.devices.Create(ctx, d); err != nil {
			t.Fatalf("create device: %v", err)
		}
		got, err := s.devices.GetByDeviceID(ctx, "dev1")
		if err != nil || got.FCMToken != "tok" {
			t.Fatalf("get device: %+v %v", got, err)
		}
		if err := s.devices.ClearFCMToken(ctx, "dev1"); err != nil {
			t.Fatalf("clear fcm: %v", err)
		}
		got, _ = s.devices.GetByDeviceID(ctx, "dev1")
		if got.FCMToken != "" {
			t.Fatalf("fcm after clear = %q, want empty", got.FCMToken)
		}
		list, err := s.devices.ListByUser(ctx, u.ID)
		if err != nil || len(list) != 1 {
			t.Fatalf("list devices: %d %v", len(list), err)
		}
	})

	t.Run("Fanout_Messages_ResolveRecipients", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		owner := mustUser(t, s, "owner@example.com", 0)
		recipient := mustUser(t, s, "recip@example.com", 1)
		app := mustApp(t, s, owner.ID, 0)
		now := time.Now().UTC()
		send := store.Send{AppID: app.ID, SenderUserID: owner.ID, Priority: 1, Body: "hi", Timestamp: 1, CreatedAt: now}
		msgs := []store.Message{{RecipientUserID: recipient.ID, CreatedAt: now}}
		sid, err := s.sends.CreateFanout(ctx, &store.Fanout{Send: send, Messages: msgs})
		if err != nil || sid == 0 {
			t.Fatalf("fanout: %v id=%d", err, sid)
		}
		got, err := s.sends.GetByID(ctx, sid)
		if err != nil || got.Body != "hi" {
			t.Fatalf("get send: %+v %v", got, err)
		}
		if got.HTML {
			t.Fatalf("html = true, want false")
		}
		// Resolve a user_key.
		ids, err := s.sends.ResolveRecipients(ctx, []string{recipient.UserKey})
		if err != nil || len(ids) != 1 || ids[0] != recipient.ID {
			t.Fatalf("resolve user: %v %v", ids, err)
		}
		// Unknown key -> ErrNotFound.
		if _, err := s.sends.ResolveRecipients(ctx, []string{"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resolve unknown err = %v, want ErrNotFound", err)
		}
		// Messages for recipient.
		list, err := s.messages.ListSince(ctx, recipient.ID, 0, 10)
		if err != nil || len(list) != 1 {
			t.Fatalf("list messages: %d %v", len(list), err)
		}
		if err := s.messages.MarkDelivered(ctx, list[0].ID, now); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
		// Idempotent mark.
		_ = s.messages.MarkDelivered(ctx, list[0].ID, now.Add(time.Second))
		mx, _ := s.messages.MaxID(ctx, recipient.ID)
		if mx != list[0].ID {
			t.Fatalf("max id = %d, want %d", mx, list[0].ID)
		}
	})

	t.Run("Ingest_WithAttachment", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		owner := mustUser(t, s, "ingest@example.com", 0)
		app := mustApp(t, s, owner.ID, 1)
		now := time.Now().UTC()
		in := &store.IngestInput{
			Send:       store.Send{AppID: app.ID, SenderUserID: owner.ID, Priority: 0, Body: "attach", Timestamp: 1, CreatedAt: now},
			Messages:   []store.Message{{RecipientUserID: owner.ID, CreatedAt: now}},
			Attachment: &store.Attachment{ContentType: "image/png", Data: []byte{1, 2, 3, 4}},
		}
		sid, err := s.ing.Ingest(ctx, in)
		if err != nil || sid == 0 {
			t.Fatalf("ingest: %v id=%d", err, sid)
		}
		att, err := s.atts.GetBySendID(ctx, sid)
		if err != nil {
			t.Fatalf("get attachment: %v", err)
		}
		if att.ContentType != "image/png" || len(att.Data) != 4 {
			t.Fatalf("attachment = %+v", att)
		}
	})

	t.Run("Receipts_AckAndSweep", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		owner := mustUser(t, s, "rcpt@example.com", 0)
		app := mustApp(t, s, owner.ID, 2)
		now := time.Now().UTC()
		rid := keyN("r", 0)
		in := &store.IngestInput{
			Send:     store.Send{AppID: app.ID, SenderUserID: owner.ID, Priority: 2, ReceiptID: rid, CreatedAt: now},
			Messages: []store.Message{{RecipientUserID: owner.ID, CreatedAt: now}},
			Receipt:  &store.Receipt{ID: rid, State: "pending", ExpiresAt: ptrTime(now.Add(time.Hour))},
		}
		sid, err := s.ing.Ingest(ctx, in)
		if err != nil {
			t.Fatalf("ingest receipt: %v", err)
		}
		in.Receipt.SendID = sid
		// Acknowledge.
		if err := s.receipts.Acknowledge(ctx, rid, "u", "d", now); err != nil {
			t.Fatalf("ack: %v", err)
		}
		got, _ := s.receipts.GetByID(ctx, rid)
		if got.State != "acknowledged" {
			t.Fatalf("state = %q, want acknowledged", got.State)
		}
		// Idempotent re-ack preserves original.
		_ = s.receipts.Acknowledge(ctx, rid, "u2", "d2", now.Add(time.Second))
		got, _ = s.receipts.GetByID(ctx, rid)
		if got.AcknowledgedBy != "u" {
			t.Fatalf("ack-by changed to %q (not idempotent)", got.AcknowledgedBy)
		}
		// Sweep with zero retention sweeps everything terminal.
		res, err := s.receipts.SweepReceipts(ctx, now.Add(2*time.Hour), 0)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Receipts != 1 {
			t.Fatalf("sweep receipts = %d, want 1", res.Receipts)
		}
		if _, err := s.receipts.GetByID(ctx, rid); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("after sweep get err = %v, want ErrNotFound", err)
		}
	})

	t.Run("Quota_Upsert", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		u := mustUser(t, s, "quota@example.com", 0)
		c1, err := s.quota.Increment(ctx, u.ID, "2026-08", 5)
		if err != nil || c1 != 5 {
			t.Fatalf("increment 5: %v %d", err, c1)
		}
		c2, err := s.quota.Increment(ctx, u.ID, "2026-08", 5)
		if err != nil || c2 != 10 {
			t.Fatalf("increment 5 again: %v %d", err, c2)
		}
		got, err := s.quota.Get(ctx, u.ID, "2026-08")
		if err != nil || got.Count != 10 {
			t.Fatalf("get: %+v %v", got, err)
		}
		// Untouched period -> zero, no error.
		zero, err := s.quota.Get(ctx, u.ID, "2026-09")
		if err != nil || zero.Count != 0 {
			t.Fatalf("untouched: %+v %v", zero, err)
		}
	})

	t.Run("Timers_ClaimExclusive", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		past := time.Now().UTC().Add(-time.Minute)
		future := time.Now().UTC().Add(time.Hour)
		t1 := &store.Timer{Kind: "retry", FireAt: past, Payload: "a"}
		t2 := &store.Timer{Kind: "retry", FireAt: past, Payload: "b"}
		t3 := &store.Timer{Kind: "retry", FireAt: future, Payload: "c"}
		for _, tm := range []*store.Timer{t1, t2, t3} {
			if _, err := s.timers.Create(ctx, tm); err != nil {
				t.Fatalf("create timer: %v", err)
			}
		}
		claimed, err := s.timers.ClaimDue(ctx, time.Now().UTC(), 10)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(claimed) != 2 {
			t.Fatalf("claimed %d, want 2 (future excluded)", len(claimed))
		}
		// Re-claim finds nothing (already claimed).
		again, err := s.timers.ClaimDue(ctx, time.Now().UTC(), 10)
		if err != nil {
			t.Fatalf("re-claim: %v", err)
		}
		if len(again) != 0 {
			t.Fatalf("re-claim got %d, want 0 (exclusive)", len(again))
		}
	})

	t.Run("Callbacks_DLQ", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		owner := mustUser(t, s, "cb@example.com", 0)
		app := mustApp(t, s, owner.ID, 3)
		rid := keyN("c", 0)
		in := &store.IngestInput{
			Send:     store.Send{AppID: app.ID, SenderUserID: owner.ID, Priority: 2, ReceiptID: rid, CreatedAt: time.Now().UTC()},
			Messages: []store.Message{{RecipientUserID: owner.ID, CreatedAt: time.Now().UTC()}},
			Receipt:  &store.Receipt{ID: rid, State: "pending"},
		}
		if _, err := s.ing.Ingest(ctx, in); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		cb := &store.Callback{ReceiptID: rid, URL: "https://example.com/hook", State: "pending"}
		cid, err := s.callbacks.Create(ctx, cb)
		if err != nil || cid == 0 {
			t.Fatalf("create callback: %v %d", err, cid)
		}
		if _, err := s.callbacks.GetByID(ctx, cid); err != nil {
			t.Fatalf("get callback: %v", err)
		}
		did, err := s.callbacks.CreateDLQ(ctx, &store.DLQ{CallbackID: cid, LastError: "boom", At: time.Now().UTC(), Attempts: 1})
		if err != nil || did == 0 {
			t.Fatalf("create dlq: %v %d", err, did)
		}
		dlqs, err := s.callbacks.ListDLQForCallback(ctx, cid)
		if err != nil || len(dlqs) != 1 {
			t.Fatalf("list dlq: %d %v", len(dlqs), err)
		}
	})

	t.Run("Groups_Members", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		owner := mustUser(t, s, "gowner@example.com", 0)
		m1 := mustUser(t, s, "gm1@example.com", 1)
		m2 := mustUser(t, s, "gm2@example.com", 2)
		g := &store.Group{UserID: owner.ID, GroupKey: keyN("g", 0), Name: "team", CreatedAt: time.Now().UTC()}
		gid, err := s.groups.Create(ctx, g)
		if err != nil || gid == 0 {
			t.Fatalf("create group: %v %d", err, gid)
		}
		if err := s.groups.SetMembers(ctx, gid, []int64{m1.ID, m2.ID}, nil); err != nil {
			t.Fatalf("set members: %v", err)
		}
		// Duplicate add is a no-op (ON CONFLICT DO NOTHING).
		_ = s.groups.SetMembers(ctx, gid, []int64{m1.ID}, nil)
		ids, err := s.groups.ListMemberIDs(ctx, gid)
		if err != nil || len(ids) != 2 {
			t.Fatalf("members: %v %d", err, len(ids))
		}
		keys, err := s.groups.ListMemberKeys(ctx, gid)
		if err != nil || len(keys) != 2 {
			t.Fatalf("member keys: %v %d", err, len(keys))
		}
		// Resolve group_key expands to members.
		recipients, err := s.sends.ResolveRecipients(ctx, []string{g.GroupKey})
		if err != nil || len(recipients) != 2 {
			t.Fatalf("resolve group: %v %d", err, len(recipients))
		}
		// Update + delete.
		if err := s.groups.Update(ctx, gid, "team2", "memo"); err != nil {
			t.Fatalf("update group: %v", err)
		}
		gg, _ := s.groups.GetByKey(ctx, g.GroupKey)
		if gg.Name != "team2" {
			t.Fatalf("name = %q", gg.Name)
		}
		if err := s.groups.Delete(ctx, gid); err != nil {
			t.Fatalf("delete group: %v", err)
		}
		if _, err := s.groups.GetByID(ctx, gid); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("after delete err = %v", err)
		}
	})

	t.Run("Subscriptions_ApproveMigrate", func(t *testing.T) {
		ctx := context.Background()
		s := freshStore(t, dsn)
		owner := mustUser(t, s, "subowner@example.com", 0)
		subscriber := mustUser(t, s, "subber@example.com", 1)
		appA := mustApp(t, s, owner.ID, 10)
		appB := mustApp(t, s, owner.ID, 11)
		sub := &store.Subscription{AppID: appA.ID, OwnerUserID: owner.ID, SubscriptionCode: keyN("s", 0), CreatedAt: time.Now().UTC()}
		sid, err := s.subs.Create(ctx, sub)
		if err != nil || sid == 0 {
			t.Fatalf("create sub: %v %d", err, sid)
		}
		if _, err := s.subs.GetByCode(ctx, sub.SubscriptionCode); err != nil {
			t.Fatalf("get by code: %v", err)
		}
		k1, err := s.subs.Approve(ctx, sid, appA.ID, subscriber.ID)
		if err != nil {
			t.Fatalf("approve: %v", err)
		}
		// Idempotent: re-approve returns the same key.
		k2, err := s.subs.Approve(ctx, sid, appA.ID, subscriber.ID)
		if err != nil || k2.SubscribedKey != k1.SubscribedKey {
			t.Fatalf("re-approve: %v (keys %q vs %q)", err, k1.SubscribedKey, k2.SubscribedKey)
		}
		// Resolves like a user_key.
		recipients, err := s.sends.ResolveRecipients(ctx, []string{k1.SubscribedKey})
		if err != nil || len(recipients) != 1 || recipients[0] != subscriber.ID {
			t.Fatalf("resolve sub key: %v %v", recipients, err)
		}
		// Migrate to appB invalidates old keys.
		n, err := s.subs.Migrate(ctx, sid, appA.ID, appB.ID)
		if err != nil || n != 1 {
			t.Fatalf("migrate: %v n=%d", err, n)
		}
		// Old key no longer resolves.
		if _, err := s.sends.ResolveRecipients(ctx, []string{k1.SubscribedKey}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("old sub key after migrate err = %v, want ErrNotFound", err)
		}
		// New key resolves (look it up by appB+user).
		newKey, err := s.subs.keyByAppUser(ctx, appB.ID, subscriber.ID)
		if err != nil {
			t.Fatalf("key by appB: %v", err)
		}
		recipients, err = s.sends.ResolveRecipients(ctx, []string{newKey.SubscribedKey})
		if err != nil || len(recipients) != 1 || recipients[0] != subscriber.ID {
			t.Fatalf("resolve new sub key: %v %v", recipients, err)
		}
	})
}

// ----- test helpers -----

// keyN returns a deterministic, unique 30-char [a-z] key seeded by seed and
// disambiguated by n, so tests do not depend on crypto/rand for setup rows.
func keyN(seed string, n int) string {
	const n30 = 30
	out := make([]byte, n30)
	for i := 0; i < n30; i++ {
		out[i] = 'a' + byte((seed[0]-'a'+byte(n)+byte(i))%26)
	}
	return string(out)
}

func mustUser(t *testing.T, s *Store, email string, n int) store.User {
	t.Helper()
	u := &store.User{Email: email, PassHash: "h", UserKey: keyN(email, n), CreatedAt: time.Now().UTC()}
	id, err := s.users.CreateBootstrap(context.Background(), u)
	if err != nil {
		t.Fatalf("create user %q: %v", email, err)
	}
	u.ID = id
	return *u
}

func mustApp(t *testing.T, s *Store, userID int64, n int) store.App {
	t.Helper()
	a := &store.App{UserID: userID, Token: keyN("token", n), Name: "app"}
	id, err := s.apps.Create(context.Background(), a)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	a.ID = id
	return *a
}

func ptrTime(t time.Time) *time.Time { return &t }
