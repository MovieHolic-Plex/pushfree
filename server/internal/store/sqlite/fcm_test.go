package sqlite

import (
	"context"
	"testing"

	"github.com/pushfree/pushfree/internal/store"
)

// TestClearFCMToken verifies the token-invalidation path the FCM channel
// (todo 16) relies on: nulling devices.fcm_token on a terminal delivery
// failure. It covers the happy clear, idempotency on a tokenless device, and
// no-op on an unknown device.
func TestClearFCMToken(t *testing.T) {
	s := newDB(t)
	u := mustSeedUser(t, s.users, 1, "u@x")
	devWith := store.Device{
		UserID: u.ID, DeviceID: "dev-fcm", SecretHash: "h", Name: "phone",
		Model: "Pixel", OS: "Android", FCMToken: "fcm-abc",
	}
	if _, err := s.devs.Create(context.Background(), &devWith); err != nil {
		t.Fatalf("create device with token: %v", err)
	}
	devWithout := store.Device{
		UserID: u.ID, DeviceID: "dev-none", SecretHash: "h2", Name: "tablet",
		Model: "Tab", OS: "Android",
	}
	if _, err := s.devs.Create(context.Background(), &devWithout); err != nil {
		t.Fatalf("create tokenless device: %v", err)
	}

	// Happy path: clearing a present token.
	if err := s.devs.ClearFCMToken(context.Background(), "dev-fcm"); err != nil {
		t.Fatalf("ClearFCMToken: %v", err)
	}
	got, err := s.devs.GetByDeviceID(context.Background(), "dev-fcm")
	if err != nil {
		t.Fatalf("GetByDeviceID: %v", err)
	}
	if got.FCMToken != "" {
		t.Errorf("after clear, fcm_token = %q, want empty", got.FCMToken)
	}

	// Idempotent: clearing a device that already has no token is not an error.
	if err := s.devs.ClearFCMToken(context.Background(), "dev-none"); err != nil {
		t.Errorf("clear tokenless device returned error: %v", err)
	}

	// Unknown device: also not an error (no row to update is a valid outcome;
	// the caller already knows the send failed).
	if err := s.devs.ClearFCMToken(context.Background(), "does-not-exist"); err != nil {
		t.Errorf("clear unknown device returned error: %v", err)
	}
}
