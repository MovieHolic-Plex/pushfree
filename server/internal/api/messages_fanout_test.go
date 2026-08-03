package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestFanout covers the todo 9 multi-user fan-out path: comma-separated user
// keys, group fan-out, the <=50 cap, per-member quota, and the 404 for a key
// that resolves to neither a user nor a group.
func TestFanout(t *testing.T) {
	// --- 3-member group send -> 3 message rows + quota 3 -------------------
	// This is the headline acceptance test: a single group key expands to one
	// sends row plus three messages rows (one per member), and the sender's
	// quota counter increases by exactly 3 (one per concrete recipient).
	t.Run("group_send_3_members_3_rows_quota_3", func(t *testing.T) {
		a, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		// Register three recipient users and collect their user_keys.
		var memberKeys []string
		for i := 0; i < 3; i++ {
			email := "gmember" + string(rune('1'+i)) + "@example.com"
			memberKeys = append(memberKeys, register(t, newClient(t), base, email, "password1"))
		}
		// Create a group with those members (session-auth on the sender).
		login(t, c, base, "ingest@example.com", "password1")
		gk := createGroup(t, c, base, "ops-team", "on-call rotation", strings.Join(memberKeys, ","))
		if !userKeyRe.MatchString(gk) {
			t.Fatalf("group_key %q is not 30-char [A-Za-z0-9]", gk)
		}

		// Send to the group key.
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {gk}, "message": {"group hello"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("group send: status=%d body=%s", status, raw)
		}

		// Each member has exactly one message row from this send.
		for i := 0; i < 3; i++ {
			email := "gmember" + string(rune('1'+i)) + "@example.com"
			uid := sessionUserID(t, a, email)
			msgs, err := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
			if err != nil {
				t.Fatalf("list messages for %s: %v", email, err)
			}
			if len(msgs) != 1 {
				t.Fatalf("member %s: want 1 message, got %d", email, len(msgs))
			}
		}

		// Sender's quota charged 1 per member == 3.
		senderID := sessionUserID(t, a, "ingest@example.com")
		qc, err := a.repos.Quota.Get(context.Background(), senderID, quotaPeriod(time.Now()))
		if err != nil {
			t.Fatalf("quota get: %v", err)
		}
		if qc.Count != 3 {
			t.Fatalf("quota count=%d, want 3", qc.Count)
		}
	})

	// --- multi-user comma list: 3 users -> 3 message rows ------------------
	// Sending to a comma-separated list of user keys fans out one message per
	// key. Quota charges 1 per user.
	t.Run("multi_user_comma_list", func(t *testing.T) {
		a, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		var keys []string
		for i := 0; i < 3; i++ {
			email := "fan" + string(rune('1'+i)) + "@example.com"
			keys = append(keys, register(t, newClient(t), base, email, "password1"))
		}
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {strings.Join(keys, ",")}, "message": {"hi all"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("multi-user send: status=%d body=%s", status, raw)
		}
		for i := 0; i < 3; i++ {
			email := "fan" + string(rune('1'+i)) + "@example.com"
			uid := sessionUserID(t, a, email)
			msgs, _ := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
			if len(msgs) != 1 {
				t.Fatalf("user %s: want 1 message, got %d", email, len(msgs))
			}
		}
		senderID := sessionUserID(t, a, "ingest@example.com")
		qc, _ := a.repos.Quota.Get(context.Background(), senderID, quotaPeriod(time.Now()))
		if qc.Count != 3 {
			t.Fatalf("quota count=%d, want 3", qc.Count)
		}
	})

	// --- 51 user keys -> 400 (the <=50 cap) --------------------------------
	// The comma list is capped at 50 keys; 51 is a validation error.
	t.Run("fifty_one_users_400", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		// Build 51 keys by repeating the sender's own key (format-valid; we
		// only test the count cap, not resolution).
		keys := make([]string, 51)
		for i := range keys {
			keys[i] = userKey
		}
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {strings.Join(keys, ",")}, "message": {"m"},
		})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
	})

	// --- 50 user keys -> accepted ------------------------------------------
	// Exactly 50 keys is the boundary: accepted.
	t.Run("fifty_users_accepted", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		keys := make([]string, 50)
		for i := range keys {
			keys[i] = userKey
		}
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {strings.Join(keys, ",")}, "message": {"m"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("50-key send should be accepted: status=%d body=%s", status, raw)
		}
	})

	// --- nonexistent group key -> 404 --------------------------------------
	// A key that matches no user and no group is "not found".
	t.Run("nonexistent_group_key_404", func(t *testing.T) {
		_, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"}, "message": {"m"},
		})
		requireEnvelope(t, status, body, raw, http.StatusNotFound)
	})

	// --- user+group mix in one send ----------------------------------------
	// A comma list may mix user keys and group keys; all expand correctly.
	t.Run("user_and_group_mix", func(t *testing.T) {
		a, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		login(t, c, base, "ingest@example.com", "password1")

		// Group with 2 members.
		gMember1 := register(t, newClient(t), base, "mixg1@example.com", "password1")
		gMember2 := register(t, newClient(t), base, "mixg2@example.com", "password1")
		gk := createGroup(t, c, base, "mix-group", "", strings.Join([]string{gMember1, gMember2}, ","))

		// One standalone user.
		soloKey := register(t, newClient(t), base, "mixsolo@example.com", "password1")

		// Send to "soloKey,groupKey" -> 3 recipients (1 solo + 2 group members).
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {soloKey + "," + gk}, "message": {"mix"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("mix send: status=%d body=%s", status, raw)
		}

		for _, email := range []string{"mixsolo@example.com", "mixg1@example.com", "mixg2@example.com"} {
			uid := sessionUserID(t, a, email)
			msgs, _ := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
			if len(msgs) != 1 {
				t.Fatalf("%s: want 1 message, got %d", email, len(msgs))
			}
		}
		senderID := sessionUserID(t, a, "ingest@example.com")
		qc, _ := a.repos.Quota.Get(context.Background(), senderID, quotaPeriod(time.Now()))
		if qc.Count != 3 {
			t.Fatalf("quota count=%d, want 3 (1 solo + 2 group members)", qc.Count)
		}
	})
}

// createGroup posts to /1/groups.json (session-auth) and returns the new
// group_key, failing on any non-success envelope.
func createGroup(t *testing.T, c *http.Client, baseURL, name, memo, members string) string {
	t.Helper()
	status, _, body, raw := doReq(t, c, http.MethodPost, baseURL+"/1/groups.json", map[string]any{
		"name":    name,
		"memo":    memo,
		"members": members,
	})
	if status != http.StatusOK {
		t.Fatalf("create group %q: status=%d body=%s", name, status, raw)
	}
	if body["status"] != float64(1) {
		t.Fatalf("create group status field=%v, want 1: %s", body["status"], raw)
	}
	gk, _ := body["group_key"].(string)
	if gk == "" {
		t.Fatalf("create group returned empty group_key: %s", raw)
	}
	return gk
}
