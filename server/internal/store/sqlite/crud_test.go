package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// TestUserCRUD covers create + all three lookups + nullable quiet-hours.
func TestUserCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)

	created := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	in := store.User{
		Email:      "alice@example.com",
		PassHash:   "$argon2id$v=19$...",
		Role:       "admin",
		UserKey:    key30(11),
		QuietStart: "22:00",
		QuietEnd:   "07:00",
		QuietTZ:    "America/Chicago",
		CreatedAt:  created,
	}
	id, err := s.users.Create(ctx, &in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == 0 {
		t.Fatal("Create returned id 0")
	}

	// GetByID round-trips every field, including nullable TEXT and time.
	got, err := s.users.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Email != in.Email || got.Role != in.Role || got.UserKey != in.UserKey {
		t.Fatalf("GetByID mismatch: got %+v want %+v", got, in)
	}
	if got.QuietStart != "22:00" || got.QuietEnd != "07:00" || got.QuietTZ != "America/Chicago" {
		t.Fatalf("quiet fields mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at round-trip: got %v want %v", got.CreatedAt, created)
	}

	// Lookups by both unique keys.
	if _, err := s.users.GetByEmail(ctx, "alice@example.com"); err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if _, err := s.users.GetByUserKey(ctx, key30(11)); err != nil {
		t.Fatalf("GetByUserKey: %v", err)
	}

	// Empty quiet fields survive as NULL -> "".
	plain := store.User{Email: "bob@example.com", PassHash: "h", Role: "user", UserKey: key30(12), CreatedAt: created}
	bid, err := s.users.Create(ctx, &plain)
	if err != nil {
		t.Fatalf("Create bob: %v", err)
	}
	bob, err := s.users.GetByID(ctx, bid)
	if err != nil {
		t.Fatalf("GetByID bob: %v", err)
	}
	if bob.QuietStart != "" || bob.QuietEnd != "" || bob.QuietTZ != "UTC" {
		t.Fatalf("empty quiet default mismatch: %+v", bob)
	}
}

// TestAppCRUD covers create + lookup by id and by token.
func TestAppCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	u := mustSeedUser(t, s.users, 21, "a@example.com")

	in := store.App{UserID: u.ID, Token: key30(31), Name: "monitoring"}
	id, err := s.apps.Create(ctx, &in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.apps.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.UserID != u.ID || got.Token != key30(31) || got.Name != "monitoring" {
		t.Fatalf("GetByID mismatch: %+v", got)
	}
	byToken, err := s.apps.GetByToken(ctx, key30(31))
	if err != nil {
		t.Fatalf("GetByToken: %v", err)
	}
	if byToken.ID != id {
		t.Fatalf("GetByToken id mismatch: %d vs %d", byToken.ID, id)
	}
}

// TestDeviceCRUD covers create + lookup by device_id, including NULL fcm_token.
func TestDeviceCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	u := mustSeedUser(t, s.users, 22, "d@example.com")

	// Without FCM token (NULL).
	d1 := store.Device{UserID: u.ID, DeviceID: "dev-null", SecretHash: "h1", Name: "phone", Model: "Pixel", OS: "Android"}
	if _, err := s.devs.Create(ctx, &d1); err != nil {
		t.Fatalf("Create d1: %v", err)
	}
	got, err := s.devs.GetByDeviceID(ctx, "dev-null")
	if err != nil {
		t.Fatalf("GetByDeviceID d1: %v", err)
	}
	if got.FCMToken != "" {
		t.Fatalf("expected empty fcm token, got %q", got.FCMToken)
	}

	// With FCM token.
	d2 := store.Device{UserID: u.ID, DeviceID: "dev-fcm", SecretHash: "h2", Name: "tablet", Model: "Tab", OS: "Android", FCMToken: "fcm-abc"}
	if _, err := s.devs.Create(ctx, &d2); err != nil {
		t.Fatalf("Create d2: %v", err)
	}
	got2, err := s.devs.GetByDeviceID(ctx, "dev-fcm")
	if err != nil {
		t.Fatalf("GetByDeviceID d2: %v", err)
	}
	if got2.FCMToken != "fcm-abc" {
		t.Fatalf("fcm token mismatch: %q", got2.FCMToken)
	}
}

// TestAttachmentCRUD covers the 1:1 send attachment round-trip (blob + NULL
// downloaded_at).
func TestAttachmentCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	sender := mustSeedUser(t, s.users, 23, "s@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 33)
	// Create a bare send to attach to.
	sendID, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send:     store.Send{AppID: app.ID, SenderUserID: sender.ID, Body: "hi", CreatedAt: time.Now()},
		Messages: nil,
	})
	if err != nil {
		t.Fatalf("seed send: %v", err)
	}

	data := []byte{0x89, 0x50, 0x4E, 0x47}
	att := store.Attachment{SendID: sendID, ContentType: "image/png", Data: data}
	if _, err := s.atts.Create(ctx, &att); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.atts.GetBySendID(ctx, sendID)
	if err != nil {
		t.Fatalf("GetBySendID: %v", err)
	}
	if got.ContentType != "image/png" || len(got.Data) != len(data) || got.Data[0] != 0x89 {
		t.Fatalf("attachment round-trip mismatch: %+v", got)
	}
	if got.DownloadedAt != nil {
		t.Fatalf("expected nil downloaded_at, got %v", got.DownloadedAt)
	}
}

// TestQuotaCRUD covers increment (upsert) and read, including the
// never-touched zero case.
func TestQuotaCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	u := mustSeedUser(t, s.users, 24, "q@example.com")

	// Never touched -> zero count, no error.
	c, err := s.quota.Get(ctx, u.ID, "2026-01")
	if err != nil {
		t.Fatalf("Get untouched: %v", err)
	}
	if c.Count != 0 {
		t.Fatalf("untouched count = %d, want 0", c.Count)
	}

	after1, err := s.quota.Increment(ctx, u.ID, "2026-01", 1)
	if err != nil {
		t.Fatalf("Increment #1: %v", err)
	}
	if after1 != 1 {
		t.Fatalf("after1 = %d, want 1", after1)
	}
	after2, err := s.quota.Increment(ctx, u.ID, "2026-01", 4)
	if err != nil {
		t.Fatalf("Increment #2: %v", err)
	}
	if after2 != 5 {
		t.Fatalf("after2 = %d, want 5", after2)
	}

	got, err := s.quota.Get(ctx, u.ID, "2026-01")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Count != 5 {
		t.Fatalf("persisted count = %d, want 5", got.Count)
	}

	// Different period is independent.
	other, err := s.quota.Get(ctx, u.ID, "2026-02")
	if err != nil {
		t.Fatalf("Get other period: %v", err)
	}
	if other.Count != 0 {
		t.Fatalf("other period count = %d, want 0", other.Count)
	}
}

// TestCallbackAndDLQCRUD covers callback create/get plus the dlq history list
// (the dlq table has no standalone repo; it lives on CallbackRepo).
func TestCallbackAndDLQCRUD(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	sender := mustSeedUser(t, s.users, 25, "cb@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 34)
	_ = app
	// Priority-2 send with a receipt to hang the callback on.
	r := &store.Receipt{ID: key30(51), State: "pending", ExpiresAt: ptrTime(time.Now().Add(time.Hour))}
	sendID, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send:     store.Send{AppID: app.ID, SenderUserID: sender.ID, Priority: 2, Body: "boom", CreatedAt: time.Now()},
		Messages: nil,
		Receipt:  r,
	})
	if err != nil {
		t.Fatalf("seed fanout: %v", err)
	}

	cb := store.Callback{ReceiptID: key30(51), URL: "https://example.com/cb", State: "pending"}
	id, err := s.cbs.Create(ctx, &cb)
	if err != nil {
		t.Fatalf("Create callback: %v", err)
	}
	got, err := s.cbs.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.URL != "https://example.com/cb" || got.ReceiptID != key30(51) {
		t.Fatalf("callback mismatch: %+v", got)
	}

	// Two dlq entries, ordered oldest-first by id.
	dlq1 := store.DLQ{CallbackID: id, LastError: "500 internal", At: time.Now(), Attempts: 1}
	dlq2 := store.DLQ{CallbackID: id, LastError: "timeout", At: time.Now(), Attempts: 2}
	if _, err := s.cbs.CreateDLQ(ctx, &dlq1); err != nil {
		t.Fatalf("CreateDLQ #1: %v", err)
	}
	if _, err := s.cbs.CreateDLQ(ctx, &dlq2); err != nil {
		t.Fatalf("CreateDLQ #2: %v", err)
	}
	list, err := s.cbs.ListDLQForCallback(ctx, id)
	if err != nil {
		t.Fatalf("ListDLQForCallback: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("dlq len = %d, want 2", len(list))
	}
	if list[0].LastError != "500 internal" || list[1].LastError != "timeout" {
		t.Fatalf("dlq order mismatch: %+v", list)
	}
	// SendID sanity (unused but proves the receipt/send linkage survived).
	_ = sendID
}

// TestReceiptGet covers standalone receipt create + get, exercising the many
// nullable time columns.
func TestReceiptGet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	sender := mustSeedUser(t, s.users, 26, "r@example.com")
	app := mustSeedApp(t, s.apps, sender.ID, 35)
	exp := time.Now().Add(2 * time.Hour).UTC()
	in := &store.Receipt{ID: key30(61), State: "pending", ExpiresAt: &exp}
	if _, err := s.sends.CreateFanout(ctx, &store.Fanout{
		Send:     store.Send{AppID: app.ID, SenderUserID: sender.ID, Priority: 2, Body: "x", CreatedAt: time.Now()},
		Receipt:  in,
	}); err != nil {
		t.Fatalf("seed fanout: %v", err)
	}
	got, err := s.rcpts.GetByID(ctx, key30(61))
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.State != "pending" {
		t.Fatalf("state = %q", got.State)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expires_at round-trip: got %v want %v", got.ExpiresAt, exp)
	}
	if got.AcknowledgedAt != nil || got.CanceledAt != nil {
		t.Fatalf("nullable times should be nil: %+v", got)
	}
}

// TestGetNotFound asserts the not-found sentinel surfaces from every repo.
func TestGetNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newDB(t)
	cases := []struct {
		name string
		fn   func() error
	}{
		{"user", func() error { _, err := s.users.GetByID(ctx, 999999); return err }},
		{"app", func() error { _, err := s.apps.GetByToken(ctx, key30(777)); return err }},
		{"device", func() error { _, err := s.devs.GetByDeviceID(ctx, "nope"); return err }},
		{"send", func() error { _, err := s.sends.GetByID(ctx, 999999); return err }},
		{"receipt", func() error { _, err := s.rcpts.GetByID(ctx, key30(778)); return err }},
		{"attachment", func() error { _, err := s.atts.GetBySendID(ctx, 999999); return err }},
		{"callback", func() error { _, err := s.cbs.GetByID(ctx, 999999); return err }},
	}
	for _, c := range cases {
		if err := c.fn(); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", c.name, err)
		}
	}
}
