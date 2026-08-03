package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// sha256Hex mirrors hub.hashSecret: devices.secret_hash stores the lower-case
// hex SHA-256 of the plaintext secret. Duplicated here (not imported from hub)
// so the api package's test does not depend on the hub package's internals.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// registerDeviceSecret inserts a device for userID with a KNOWN plaintext
// secret (hashed exactly as /1/devices/login.json stores it), so a test can
// ack a receipt with that secret. Returns nothing; the device becomes a valid
// ack principal.
func registerDeviceSecret(t *testing.T, a *Accounts, userID int64, deviceID, secret, name string) {
	t.Helper()
	if _, err := a.repos.Devices.Create(context.Background(), &store.Device{
		UserID:     userID,
		DeviceID:   deviceID,
		SecretHash: sha256Hex(secret),
		Name:       name,
	}); err != nil {
		t.Fatalf("register device %q: %v", deviceID, err)
	}
}

// sendPriority2 posts a priority=2 /1/messages.json send and returns the
// 30-char receipt id in the response envelope. (Named sendPriority2 by
// convention; the concurrent cancel tests use sendP2WithTag to avoid a clash.)
func sendPriority2(t *testing.T, c *http.Client, baseURL, tok, userKey, msg string) string {
	t.Helper()
	status, _, body, raw := postMessages(t, c, baseURL, url.Values{
		"token": {tok}, "user": {userKey}, "message": {msg}, "priority": {"2"},
	})
	if status != http.StatusOK {
		t.Fatalf("p2 send status=%d want 200; body=%s", status, raw)
	}
	r, _ := body["receipt"].(string)
	if !userKeyRe.MatchString(r) || len(r) != 30 {
		t.Fatalf("p2 receipt %q not 30-char [A-Za-z0-9]: %s", r, raw)
	}
	return r
}

// getReceipt polls GET /1/receipts/{receipt}.json?token= and returns the
// status code, decoded body, and raw bytes (raw is asserted directly as the
// misleading_success_output guard).
func getReceipt(t *testing.T, c *http.Client, baseURL, receipt, token string) (int, map[string]any, []byte) {
	t.Helper()
	resp, err := c.Get(baseURL + "/1/receipts/" + receipt + ".json?token=" + token)
	if err != nil {
		t.Fatalf("GET receipt: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, raw
}

// postAck POSTs a urlencoded body to /1/receipts/{receipt}/acknowledge.json.
func postAck(t *testing.T, c *http.Client, baseURL, receipt string, vals url.Values) (int, map[string]any, []byte) {
	t.Helper()
	resp, err := c.PostForm(baseURL+"/1/receipts/"+receipt+"/acknowledge.json", vals)
	if err != nil {
		t.Fatalf("POST ack: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, raw
}

// errHookUnreachable is a sentinel a failing fakeAckHook returns, to prove a
// broken callback hook never fails the HTTP ack (todo 25 owns retries/DLQ).
var errHookUnreachable = &unreachableHookErr{}

type unreachableHookErr struct{}

func (*unreachableHookErr) Error() string { return "ack hook unreachable (test)" }

// fakeAckHook records the receipt ids it was asked to acknowledge, for the
// todo-25 callback-worker seam test.
type fakeAckHook struct {
	called  []string
	failErr error
}

func (f *fakeAckHook) OnAcknowledged(_ context.Context, receiptID string) error {
	f.called = append(f.called, receiptID)
	return f.failErr
}

// TestReceipts is the todo-23 acceptance surface: ack, receipt lookup, and GC.
// It asserts against raw JSON bytes (not only decoded maps) so a handler that
// writes a success status while omitting a field is caught.
func TestReceipts(t *testing.T) {
	// --- happy: send p=2 -> poll -> ack (device secret) -> poll acknowledged ---
	t.Run("ack_via_device_secret_then_poll", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "ack@example.com")
		tok := createApp(t, c, base, "monitoring")
		userKey := userKeyFor(t, a, "ack@example.com")
		owner := sessionUserID(t, a, "ack@example.com")

		// The recipient registers a device (out-of-band); its secret acks.
		const devSecret = "S0123456789012345678901234567" // 30 chars
		registerDeviceSecret(t, a, owner, "device-ack-1", devSecret, "phone")

		receipt := sendPriority2(t, c, base, tok, userKey, "emergency")

		// Before ack: GET shows an un-acknowledged receipt.
		status, body, raw := getReceipt(t, c, base, receipt, tok)
		if status != http.StatusOK {
			t.Fatalf("GET receipt status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("GET receipt status field=%v want 1: %s", body["status"], raw)
		}
		if body["acknowledged"] != float64(0) {
			t.Fatalf("pre-ack acknowledged=%v want 0: %s", body["acknowledged"], raw)
		}

		// Ack via device_id + secret.
		astatus, abody, araw := postAck(t, c, base, receipt, url.Values{
			"device_id": {"device-ack-1"}, "secret": {devSecret},
		})
		if astatus != http.StatusOK {
			t.Fatalf("ack status=%d want 200; body=%s", astatus, araw)
		}
		if abody["status"] != float64(1) {
			t.Fatalf("ack status field=%v want 1: %s", abody["status"], araw)
		}
		// Raw-sequence guard: the ack response itself reflects acknowledged.
		if !strings.Contains(string(araw), `"acknowledged":1`) {
			t.Fatalf("ack raw body missing \"acknowledged\":1: %s", araw)
		}
		if abody["acknowledged_by"] != userKey {
			t.Fatalf("acknowledged_by=%v want %q: %s", abody["acknowledged_by"], userKey, araw)
		}
		if abody["acknowledged_by_device"] != "phone" {
			t.Fatalf("acknowledged_by_device=%v want phone: %s", abody["acknowledged_by_device"], araw)
		}
		ackAt, _ := abody["acknowledged_at"].(float64)
		if ackAt <= 0 {
			t.Fatalf("acknowledged_at missing/non-positive: %s", araw)
		}

		// Poll again: the receipt now reflects the acknowledged state and the
		// SAME acknowledged_at (idempotent read).
		_, body2, raw2 := getReceipt(t, c, base, receipt, tok)
		if body2["acknowledged"] != float64(1) {
			t.Fatalf("post-ack poll acknowledged=%v want 1: %s", body2["acknowledged"], raw2)
		}
		if body2["acknowledged_at"] != ackAt {
			t.Fatalf("poll acknowledged_at=%v want %v (must match ack): %s", body2["acknowledged_at"], ackAt, raw2)
		}

		// Store state agrees (no misleading success output).
		rc, err := a.repos.Receipts.GetByID(context.Background(), receipt)
		if err != nil {
			t.Fatalf("store get receipt: %v", err)
		}
		if rc.State != "acknowledged" {
			t.Fatalf("store state=%q want acknowledged", rc.State)
		}
	})

	// --- failure: ack with the wrong secret -> 401 + receipt unchanged ---
	t.Run("ack_wrong_secret_401_unchanged", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "wrong@example.com")
		tok := createApp(t, c, base, "monitoring")
		userKey := userKeyFor(t, a, "wrong@example.com")
		receipt := sendPriority2(t, c, base, tok, userKey, "emergency")

		status, body, raw := postAck(t, c, base, receipt, url.Values{
			"device_id": {"device-x"}, "secret": {"WRONGWRONGWRONGWRONGWRONGWRONG"},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("wrong-secret ack status=%d want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("wrong-secret status field=%v want 0: %s", body["status"], raw)
		}
		errs, _ := body["errors"].([]any)
		if len(errs) == 0 {
			t.Fatalf("wrong-secret body missing errors: %s", raw)
		}

		// Raw-sequence guard: the receipt is unchanged after the failed ack.
		_, poll, praw := getReceipt(t, c, base, receipt, tok)
		if poll["acknowledged"] != float64(0) {
			t.Fatalf("receipt changed after failed ack: %s", praw)
		}
		rc, _ := a.repos.Receipts.GetByID(context.Background(), receipt)
		if rc.State != "pending" {
			t.Fatalf("store state=%q want pending after failed ack", rc.State)
		}
	})

	// --- ack via the app token (sender) also works ---
	t.Run("ack_via_app_token", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "token-ack@example.com")
		tok := createApp(t, c, base, "monitoring")
		userKey := userKeyFor(t, a, "token-ack@example.com")
		receipt := sendPriority2(t, c, base, tok, userKey, "emergency")

		status, body, raw := postAck(t, c, base, receipt, url.Values{"token": {tok}})
		if status != http.StatusOK {
			t.Fatalf("token ack status=%d want 200; body=%s", status, raw)
		}
		if body["acknowledged"] != float64(1) {
			t.Fatalf("token ack acknowledged=%v want 1: %s", body["acknowledged"], raw)
		}
		if body["acknowledged_by"] != userKey {
			t.Fatalf("token ack acknowledged_by=%v want %q: %s", body["acknowledged_by"], userKey, raw)
		}
	})

	// --- idempotent: a second ack preserves the first acknowledged_at/by ---
	t.Run("ack_idempotent_preserves_first", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "idem@example.com")
		tok := createApp(t, c, base, "monitoring")
		userKey := userKeyFor(t, a, "idem@example.com")
		owner := sessionUserID(t, a, "idem@example.com")
		const s1 = "idem00000000000000000000000001"
		const s2 = "idem00000000000000000000000002"
		registerDeviceSecret(t, a, owner, "d1", s1, "phone")
		registerDeviceSecret(t, a, owner, "d2", s2, "laptop")
		receipt := sendPriority2(t, c, base, tok, userKey, "emergency")

		_, b1, r1 := postAck(t, c, base, receipt, url.Values{"device_id": {"d1"}, "secret": {s1}})
		if b1["acknowledged"] != float64(1) {
			t.Fatalf("first ack not acknowledged: %s", r1)
		}
		firstAt := b1["acknowledged_at"]

		// Second ack by a different device: still 200 but the ORIGINAL metadata
		// is preserved (no overwrite).
		_, b2, r2 := postAck(t, c, base, receipt, url.Values{"device_id": {"d2"}, "secret": {s2}})
		if b2["status"] != float64(1) {
			t.Fatalf("second ack status=%v want 1: %s", b2["status"], r2)
		}
		if b2["acknowledged_at"] != firstAt {
			t.Fatalf("idempotent re-ack changed acknowledged_at: %v -> %v: %s", firstAt, b2["acknowledged_at"], r2)
		}
		if b2["acknowledged_by_device"] != "phone" {
			t.Fatalf("idempotent re-ack changed by_device: %v: %s", b2["acknowledged_by_device"], r2)
		}
	})

	// --- GET receipt fields reflect the full lifecycle snapshot ---
	t.Run("get_receipt_fields_pending", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "fields@example.com")
		tok := createApp(t, c, base, "monitoring")
		userKey := userKeyFor(t, a, "fields@example.com")
		receipt := sendPriority2(t, c, base, tok, userKey, "emergency")

		_, body, raw := getReceipt(t, c, base, receipt, tok)
		// Raw-sequence guard: the pending snapshot carries the expected keys.
		for _, key := range []string{`"acknowledged":0`, `"expired":0`, `"status":1`} {
			if !strings.Contains(string(raw), key) {
				t.Fatalf("pending snapshot missing %q: %s", key, raw)
			}
		}
		if body["acknowledged_at"] != nil {
			t.Fatalf("pending acknowledged_at=%v want null: %s", body["acknowledged_at"], raw)
		}
		if _, ok := body["request"].(string); !ok {
			t.Fatalf("missing request id: %s", raw)
		}
	})

	// --- GET with a foreign token -> 404 (no cross-user enumeration) ---
	t.Run("get_receipt_foreign_token_404", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		cA := newClient(t)
		registerLogin(t, cA, base, "owner-a@example.com")
		tokA := createApp(t, cA, base, "app-a")
		keyA := userKeyFor(t, a, "owner-a@example.com")
		receipt := sendPriority2(t, cA, base, tokA, keyA, "emergency")

		// A second user's token must not see (or ack) the first user's receipt.
		cB := newClient(t)
		registerLogin(t, cB, base, "owner-b@example.com")
		tokB := createApp(t, cB, base, "app-b")

		status, _, raw := getReceipt(t, cB, base, receipt, tokB)
		if status != http.StatusNotFound {
			t.Fatalf("foreign-token GET status=%d want 404; body=%s", status, raw)
		}
		// And the foreign token cannot ack either.
		astatus, _, araw := postAck(t, cB, base, receipt, url.Values{"token": {tokB}})
		if astatus != http.StatusUnauthorized && astatus != http.StatusNotFound {
			t.Fatalf("foreign-token ack status=%d want 401/404; body=%s", astatus, araw)
		}
	})

	// --- GET unknown receipt id -> 404 ---
	t.Run("get_receipt_unknown_404", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "unk-r@example.com")
		tok := createApp(t, c, base, "monitoring")
		status, _, raw := getReceipt(t, c, base, "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", tok)
		if status != http.StatusNotFound {
			t.Fatalf("unknown receipt GET status=%d want 404; body=%s", status, raw)
		}
	})

	// --- GC: sweep >7d rows, keep fresh, with an injected clock ---
	t.Run("gc_sweeps_old_keeps_fresh", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "gc@example.com")
		createApp(t, c, base, "monitoring")
		owner := sessionUserID(t, a, "gc@example.com")
		apps, err := a.repos.Apps.ListByUser(context.Background(), owner)
		if err != nil || len(apps) == 0 {
			t.Fatalf("list apps: %v len=%d", err, len(apps))
		}
		appID := apps[0].ID

		now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
		old := now.Add(-8 * 24 * time.Hour) // > retention -> swept
		fresh := now.Add(-1 * 24 * time.Hour)

		// Generate receipt ids up front so the callback can reference the old.
		oldID, _ := newUserKey()
		freshID, _ := newUserKey()

		if _, err := a.repos.Sends.CreateFanout(context.Background(), &store.Fanout{
			Send:     store.Send{AppID: appID, SenderUserID: owner, Priority: 2, Body: "old", CreatedAt: old},
			Messages: []store.Message{{RecipientUserID: owner, CreatedAt: old}},
			Receipt:  &store.Receipt{ID: oldID, State: "acknowledged", AcknowledgedAt: &old},
		}); err != nil {
			t.Fatalf("seed old receipt: %v", err)
		}
		// A dependent callback on the doomed receipt must be cascaded.
		cbID, err := a.repos.Callbacks.Create(context.Background(), &store.Callback{
			ReceiptID: oldID, URL: "https://example.com/cb", State: "pending",
		})
		if err != nil {
			t.Fatalf("seed callback: %v", err)
		}

		if _, err := a.repos.Sends.CreateFanout(context.Background(), &store.Fanout{
			Send:     store.Send{AppID: appID, SenderUserID: owner, Priority: 2, Body: "fresh", CreatedAt: fresh},
			Messages: []store.Message{{RecipientUserID: owner, CreatedAt: fresh}},
			Receipt:  &store.Receipt{ID: freshID, State: "acknowledged", AcknowledgedAt: &fresh},
		}); err != nil {
			t.Fatalf("seed fresh receipt: %v", err)
		}

		// Inject the clock through the sweeper type (background-sweeper seam).
		sweeper := &ReceiptSweeper{
			receipts:  a.repos.Receipts,
			retention: 7 * 24 * time.Hour,
			now:       func() time.Time { return now },
		}
		res, err := sweeper.Sweep(context.Background())
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if res.Receipts != 1 {
			t.Fatalf("swept %d receipts, want 1 (the old acknowledged)", res.Receipts)
		}
		if res.Callbacks != 1 {
			t.Fatalf("swept %d callbacks, want 1 (cascade)", res.Callbacks)
		}

		// Old receipt + its callback are gone; the fresh receipt survives.
		if _, err := a.repos.Receipts.GetByID(context.Background(), oldID); err == nil {
			t.Fatalf("old receipt still present after sweep")
		}
		if _, err := a.repos.Callbacks.GetByID(context.Background(), cbID); err == nil {
			t.Fatalf("old callback still present after sweep")
		}
		if _, err := a.repos.Receipts.GetByID(context.Background(), freshID); err != nil {
			t.Fatalf("fresh receipt swept (want kept): %v", err)
		}
	})

	// --- ack hook seam: todo 25 callback worker plug-in point ---
	t.Run("ack_hook_fires_and_errors_are_non_fatal", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		hook := &fakeAckHook{failErr: nil}
		a.SetAckHook(hook)
		c := newClient(t)
		registerLogin(t, c, base, "hook@example.com")
		tok := createApp(t, c, base, "monitoring")
		userKey := userKeyFor(t, a, "hook@example.com")
		receipt := sendPriority2(t, c, base, tok, userKey, "emergency")

		status, body, raw := postAck(t, c, base, receipt, url.Values{"token": {tok}})
		if status != http.StatusOK {
			t.Fatalf("ack status=%d want 200; body=%s", status, raw)
		}
		if body["acknowledged"] != float64(1) {
			t.Fatalf("ack not acknowledged: %s", raw)
		}
		if len(hook.called) != 1 || hook.called[0] != receipt {
			t.Fatalf("ack hook called with %v, want [%s]", hook.called, receipt)
		}

		// A failing hook must NOT fail the ack (callbacks are best-effort;
		// todo 25 owns retries/DLQ).
		a.SetAckHook(&fakeAckHook{failErr: errHookUnreachable})
		status2, body2, raw2 := postAck(t, c, base, receipt, url.Values{"token": {tok}})
		if status2 != http.StatusOK {
			t.Fatalf("ack with failing hook status=%d want 200; body=%s", status2, raw2)
		}
		if body2["status"] != float64(1) {
			t.Fatalf("ack with failing hook status field=%v want 1: %s", body2["status"], raw2)
		}
	})
}
