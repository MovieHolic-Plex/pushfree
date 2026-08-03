package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSubscriptions covers the todo 12 subscription-code lifecycle with
// dynamic per-app keys: issue -> approve -> send via the dynamic key
// succeeds; keys differ per app; migrate invalidates the old key and makes the
// new one valid.
func TestSubscriptions(t *testing.T) {
	// --- happy path: issue -> approve -> send via dynamic key succeeds -----
	// The app owner creates a subscription channel; a subscriber approves via
	// their session and receives a per-app dynamic key; the owner sends to
	// that key and the message is delivered to the subscriber.
	t.Run("issue_approve_send", func(t *testing.T) {
		a, base := newAppsTestServer(t)

		// App owner: register, log in, create an app token.
		owner := newClient(t)
		registerLogin(t, owner, base, "appowner@example.com")
		tok := createApp(t, owner, base, "monitoring")

		// Subscriber: register, log in (session for authorize).
		sub := newClient(t)
		register(t, sub, base, "subscriber@example.com", "password1")
		login(t, sub, base, "subscriber@example.com", "password1")

		// Issue the subscription channel.
		status, _, body, raw := doReq(t, owner, http.MethodPost, base+"/1/subscriptions",
			map[string]any{"token": tok, "title": "ops alerts"})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("issue: status=%d body=%s", status, raw)
		}
		code, _ := body["subscription_code"].(string)
		if !userKeyRe.MatchString(code) {
			t.Fatalf("subscription_code %q is not 30-char [A-Za-z0-9]: %s", code, raw)
		}
		if su, _ := body["subscribe_url"].(string); su == "" {
			t.Fatalf("subscribe_url missing: %s", raw)
		}

		// Subscriber approves -> per-app dynamic key.
		status, _, body, raw = doReq(t, sub, http.MethodPost, base+"/1/subscriptions/authorize",
			map[string]any{"subscription_code": code})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("authorize: status=%d body=%s", status, raw)
		}
		key1, _ := body["subscribed_user_key"].(string)
		if !userKeyRe.MatchString(key1) {
			t.Fatalf("subscribed_user_key %q is not 30-char [A-Za-z0-9]: %s", key1, raw)
		}

		// Owner sends to the dynamic key; it resolves to the subscriber.
		status, _, body, raw = postMessages(t, owner, base, url.Values{
			"token": {tok}, "user": {key1}, "message": {"via dynamic key"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("send via dynamic key: status=%d body=%s", status, raw)
		}

		// The subscriber has exactly one delivered message row.
		subID := sessionUserID(t, a, "subscriber@example.com")
		msgs, err := a.repos.Messages.ListSince(context.Background(), subID, 0, 100)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("subscriber: want 1 message, got %d", len(msgs))
		}

		// Re-approving is idempotent: same app+user returns the SAME key.
		_, _, body, _ = doReq(t, sub, http.MethodPost, base+"/1/subscriptions/authorize",
			map[string]any{"subscription_code": code})
		if keyAgain, _ := body["subscribed_user_key"].(string); keyAgain != key1 {
			t.Fatalf("re-approve not stable: got %q want %q", keyAgain, key1)
		}
	})

	// --- keys differ per app ----------------------------------------------
	// The same subscriber approving two different apps receives two DIFFERENT
	// dynamic keys, both resolving to the same user.
	t.Run("keys_differ_per_app", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		owner := newClient(t)
		registerLogin(t, owner, base, "multiowner@example.com")
		tok1 := createApp(t, owner, base, "app1")
		tok2 := createApp(t, owner, base, "app2")

		sub := newClient(t)
		register(t, sub, base, "multisub@example.com", "password1")
		login(t, sub, base, "multisub@example.com", "password1")

		// Issue a subscription under each app.
		c1 := issueSubscription(t, owner, base, tok1, "ch1")
		c2 := issueSubscription(t, owner, base, tok2, "ch2")

		k1 := approve(t, sub, base, c1)
		k2 := approve(t, sub, base, c2)
		if k1 == k2 {
			t.Fatalf("keys for two different apps must differ: %q == %q", k1, k2)
		}
		// Both resolve to the same subscriber.
		subID := sessionUserID(t, a, "multisub@example.com")
		for i, k := range []string{k1, k2} {
			if _, _, body, raw := postMessages(t, owner, base, url.Values{
				"token": {[]string{tok1, tok2}[i]}, "user": {k}, "message": {"m"},
			}); body["status"] != float64(1) {
				t.Fatalf("send via key %d: %s", i+1, raw)
			}
		}
		msgs, _ := a.repos.Messages.ListSince(context.Background(), subID, 0, 100)
		if len(msgs) != 2 {
			t.Fatalf("subscriber: want 2 messages (one per app key), got %d", len(msgs))
		}
	})

	// --- migrate: old key invalid, new key valid --------------------------
	// Migrating the channel to a second app regenerates the subscriber key:
	// the old key no longer resolves (404), and re-approving yields a new,
	// valid key under the destination app.
	t.Run("migrate_old_invalid_new_valid", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		owner := newClient(t)
		registerLogin(t, owner, base, "migowner@example.com")
		tok1 := createApp(t, owner, base, "src")
		tok2 := createApp(t, owner, base, "dst")

		sub := newClient(t)
		register(t, sub, base, "migsub@example.com", "password1")
		login(t, sub, base, "migsub@example.com", "password1")

		code := issueSubscription(t, owner, base, tok1, "migratable")
		oldKey := approve(t, sub, base, code)

		// Sending via the old key works before migration.
		status, _, body, raw := postMessages(t, owner, base, url.Values{
			"token": {tok1}, "user": {oldKey}, "message": {"before"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("send before migrate: status=%d body=%s", status, raw)
		}

		// Migrate from app1 to app2.
		status, _, body, raw = doReq(t, owner, http.MethodPost, base+"/1/subscriptions/migrate.json",
			map[string]any{
				"subscription_code": code,
				"from_app_token":    tok1,
				"to_app_token":      tok2,
			})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("migrate: status=%d body=%s", status, raw)
		}
		if migrated, _ := body["migrated"].(float64); migrated != 1 {
			t.Fatalf("migrated count=%v want 1: %s", body["migrated"], raw)
		}

		// The old key is now invalid (no longer resolves).
		status, _, body, raw = postMessages(t, owner, base, url.Values{
			"token": {tok1}, "user": {oldKey}, "message": {"after"},
		})
		if status != http.StatusNotFound {
			t.Fatalf("send via old key after migrate: status=%d want 404; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("old key should be invalid: %s", raw)
		}

		// Re-approve (channel now parented on app2) yields a NEW key.
		newKey := approve(t, sub, base, code)
		if newKey == oldKey {
			t.Fatalf("migrate must regenerate the key; got same %q", newKey)
		}

		// Sending via the new key under app2 succeeds and reaches the same user.
		status, _, body, raw = postMessages(t, owner, base, url.Values{
			"token": {tok2}, "user": {newKey}, "message": {"via new key"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("send via new key: status=%d body=%s", status, raw)
		}
		subID := sessionUserID(t, a, "migsub@example.com")
		msgs, _ := a.repos.Messages.ListSince(context.Background(), subID, 0, 100)
		// before-migrate (1) + after-migrate new key (1) = 2 rows; the
		// old-key-after-migrate send produced none.
		if len(msgs) != 2 {
			t.Fatalf("subscriber: want 2 delivered messages, got %d", len(msgs))
		}
	})

	// --- create with an invalid app token -> 401 --------------------------
	t.Run("create_invalid_token_401", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		status, _, body, raw := doReq(t, c, http.MethodPost, base+"/1/subscriptions",
			map[string]any{"token": "X" + strings.Repeat("0", 29), "title": "x"})
		if status != http.StatusUnauthorized {
			t.Fatalf("invalid token: status=%d want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	// --- authorize without a session -> 401 -------------------------------
	t.Run("authorize_requires_session_401", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t) // no session cookie
		status, _, body, raw := doReq(t, c, http.MethodPost, base+"/1/subscriptions/authorize",
			map[string]any{"subscription_code": strings.Repeat("A", 30)})
		if status != http.StatusUnauthorized {
			t.Fatalf("authorize without session: status=%d want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	// --- authorize an unknown code -> 404 ---------------------------------
	t.Run("authorize_unknown_code_404", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		sub := newClient(t)
		registerLogin(t, sub, base, "unksub@example.com")
		status, _, body, raw := doReq(t, sub, http.MethodPost, base+"/1/subscriptions/authorize",
			map[string]any{"subscription_code": strings.Repeat("Z", 30)})
		if status != http.StatusNotFound {
			t.Fatalf("authorize unknown code: status=%d want 404; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	// --- migrate with mismatched app owners -> 403 ------------------------
	t.Run("migrate_mismatched_owners_403", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		ownerA := newClient(t)
		registerLogin(t, ownerA, base, "ownera@example.com")
		tokA := createApp(t, ownerA, base, "appa")
		code := issueSubscription(t, ownerA, base, tokA, "ch")

		ownerB := newClient(t)
		registerLogin(t, ownerB, base, "ownerb@example.com")
		tokB := createApp(t, ownerB, base, "appb")

		status, _, body, raw := doReq(t, ownerA, http.MethodPost, base+"/1/subscriptions/migrate.json",
			map[string]any{
				"subscription_code": code,
				"from_app_token":    tokA,
				"to_app_token":      tokB, // different owner
			})
		if status != http.StatusForbidden {
			t.Fatalf("migrate mismatched owners: status=%d want 403; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})
}

// issueSubscription posts to /1/subscriptions and returns the new code.
func issueSubscription(t *testing.T, c *http.Client, baseURL, token, title string) string {
	t.Helper()
	status, _, body, raw := doReq(t, c, http.MethodPost, baseURL+"/1/subscriptions",
		map[string]any{"token": token, "title": title})
	if status != http.StatusOK || body["status"] != float64(1) {
		t.Fatalf("issue subscription: status=%d body=%s", status, raw)
	}
	code, _ := body["subscription_code"].(string)
	if code == "" {
		t.Fatalf("issue subscription returned empty code: %s", raw)
	}
	return code
}

// approve posts to /1/subscriptions/authorize and returns the dynamic key.
func approve(t *testing.T, c *http.Client, baseURL, code string) string {
	t.Helper()
	status, _, body, raw := doReq(t, c, http.MethodPost, baseURL+"/1/subscriptions/authorize",
		map[string]any{"subscription_code": code})
	if status != http.StatusOK || body["status"] != float64(1) {
		t.Fatalf("authorize: status=%d body=%s", status, raw)
	}
	key, _ := body["subscribed_user_key"].(string)
	if key == "" {
		t.Fatalf("authorize returned empty key: %s", raw)
	}
	return key
}
