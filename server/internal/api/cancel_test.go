package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/receipts"
	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/store/sqlite"
)

// These tests are the RED phase of todo 24 (cancel + cancel_by_tag, TDD). They
// were committed BEFORE the cancel handlers/routes existed, so every case
// failed against an unregistered route (404). The GREEN implementation lands
// in a follow-up commit.
//
// Contract under test (plan todo 24 + Pushover-compatible envelope):
//   - POST /1/receipts/{receipt}/cancel.json cancels a QUEUED (pending, not-
//     yet-delivered) emergency receipt: state -> canceled (terminal, so the
//     retry scheduler returns EventDone and no further attempts fire), the
//     receipt's scheduled timers are removed, and the response is
//     {"status":1,"request":"<uuid>"}.
//   - POST /1/receipts/cancel_by_tag.json {tag} cancels exactly the pending
//     receipts carrying that tag and owned by the authenticated app; receipts
//     with other tags (or already past pending) are left untouched.
//   - Canceling a receipt that has already progressed (delivered /
//     acknowledged / expired / canceled) is an error:
//     {"status":0,"errors":[...],"request":"<uuid>"} with a non-2xx status.

// newCancelEnv is newAppsTestServer that also exposes the concrete *sqlite.Store
// so cancel tests can set up non-pending receipt states (delivered/acked) and
// inspect timer rows through raw SQL -- those state transitions and the
// timer-count read are not yet exposed as store methods (todo 22/23), and
// reaching the DB directly keeps the test free of not-yet-existing helpers.
func newCancelEnv(t *testing.T) (*Accounts, *sqlite.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pushfree-cancel-test.db")
	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := New(st.Repos(), []byte("cancel-test-secret"), 0, nil)
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, st, srv.URL
}

// cancelSetup registers+logs in one user and mints an app token, returning the
// session client, token, and user_key. messages.json and the cancel endpoints
// authenticate via the app token in the form body, so no session is needed for
// the requests under test; the session is only used to create the app.
func cancelSetup(t *testing.T, base string) (c *http.Client, token, userKey string) {
	t.Helper()
	c = newClient(t)
	userKey = register(t, c, base, "cancel@example.com", "password1")
	login(t, c, base, "cancel@example.com", "password1")
	token = createApp(t, c, base, "monitoring")
	return c, token, userKey
}

// sendP2WithTag posts a priority-2 emergency message carrying `tag` and returns
// its receipt id. (Renamed from sendPriority2 to avoid colliding with the
// same-named helper in receipts_test.go, written concurrently by worker 23.)
func sendP2WithTag(t *testing.T, c *http.Client, baseURL, token, userKey, tag string) string {
	t.Helper()
	vals := url.Values{"token": {token}, "user": {userKey}, "message": {"emergency"}}
	vals.Set("priority", "2")
	if tag != "" {
		vals.Set("tags", tag)
	}
	status, _, body, raw := postMessages(t, c, baseURL, vals)
	if status != http.StatusOK || body["status"] != float64(1) {
		t.Fatalf("send p2: status=%d body=%s", status, raw)
	}
	r, _ := body["receipt"].(string)
	if r == "" {
		t.Fatalf("p2 send missing receipt: %s", raw)
	}
	return r
}

// postCancel POSTs /1/receipts/{receipt}/cancel.json with the app token.
func postCancel(t *testing.T, c *http.Client, baseURL, receipt, token string) (int, http.Header, map[string]any, []byte) {
	t.Helper()
	resp, err := c.PostForm(baseURL+"/1/receipts/"+receipt+"/cancel.json", url.Values{"token": {token}})
	if err != nil {
		t.Fatalf("post cancel: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, resp.Header, decoded, raw
}

// postCancelByTag POSTs /1/receipts/cancel_by_tag.json with {tag, token}.
func postCancelByTag(t *testing.T, c *http.Client, baseURL, tag, token string) (int, http.Header, map[string]any, []byte) {
	t.Helper()
	resp, err := c.PostForm(baseURL+"/1/receipts/cancel_by_tag.json", url.Values{"tag": {tag}, "token": {token}})
	if err != nil {
		t.Fatalf("post cancel_by_tag: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, resp.Header, decoded, raw
}

// timerCount counts timer rows tied to receiptID via raw SQL.
func timerCount(t *testing.T, st *sqlite.Store, receiptID string) int64 {
	t.Helper()
	var n int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM timers WHERE receipt_id = ?`, receiptID).Scan(&n); err != nil {
		t.Fatalf("count timers: %v", err)
	}
	return n
}

// setReceiptState force-sets a receipt's state via raw SQL, to set up
// non-pending states (delivered/acknowledged) that no implemented code path
// produces yet (todo 23 owns the pending->delivered transition).
func setReceiptState(t *testing.T, st *sqlite.Store, receiptID, state string) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE receipts SET state = ? WHERE id = ?`, state, receiptID); err != nil {
		t.Fatalf("set receipt state: %v", err)
	}
}

func TestCancel(t *testing.T) {
	// --- queued (pending) cancel succeeds, marks canceled, stops retries ---
	t.Run("pending_cancel_succeeds_and_stops_retries", func(t *testing.T) {
		a, st, base := newCancelEnv(t)
		c, tok, userKey := cancelSetup(t, base)
		r := sendP2WithTag(t, c, base, tok, userKey, "alert")

		// Seed a retry timer for the receipt (todo 22 owns the timer engine;
		// here we simulate a scheduled retry existing before cancel).
		if _, err := a.repos.Timers.Create(context.Background(), &store.Timer{
			Kind: "retry", ReceiptID: r, FireAt: time.Now().Add(30 * time.Second),
		}); err != nil {
			t.Fatalf("seed timer: %v", err)
		}
		if n := timerCount(t, st, r); n != 1 {
			t.Fatalf("seed: want 1 timer, got %d", n)
		}

		status, _, body, raw := postCancel(t, c, base, r, tok)
		if status != http.StatusOK {
			t.Fatalf("cancel status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("status field=%v want 1: %s", body["status"], raw)
		}
		if req, _ := body["request"].(string); req == "" {
			t.Fatalf("missing request id: %s", raw)
		}

		// Receipt is now canceled (terminal) -> the retry scheduler returns
		// EventDone on its next tick, so no further delivery attempts fire.
		rc, err := a.repos.Receipts.GetByID(context.Background(), r)
		if err != nil {
			t.Fatalf("get receipt: %v", err)
		}
		if rc.State != "canceled" {
			t.Fatalf("receipt state=%q want canceled", rc.State)
		}
		if rc.CanceledAt == nil {
			t.Fatalf("canceled_at not set")
		}
		if !receipts.State(rc.State).Terminal() {
			t.Fatalf("state %q is not terminal; retries would continue", rc.State)
		}

		// The scheduled timer was removed (no future retry claim).
		if n := timerCount(t, st, r); n != 0 {
			t.Fatalf("after cancel: want 0 timers, got %d", n)
		}
	})

	// --- cancel_by_tag cancels exactly the tagged pending set --------------
	t.Run("cancel_by_tag_cancels_exact_set", func(t *testing.T) {
		a, _, base := newCancelEnv(t)
		c, tok, userKey := cancelSetup(t, base)

		// Two p2 sends tagged "ops", one tagged "other".
		ops1 := sendP2WithTag(t, c, base, tok, userKey, "ops")
		ops2 := sendP2WithTag(t, c, base, tok, userKey, "ops")
		other := sendP2WithTag(t, c, base, tok, userKey, "other")

		status, _, body, raw := postCancelByTag(t, c, base, "ops", tok)
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("cancel_by_tag: status=%d body=%s", status, raw)
		}
		canceled, _ := body["canceled"].([]any)
		got := map[string]bool{}
		for _, v := range canceled {
			got[v.(string)] = true
		}
		if !got[ops1] || !got[ops2] {
			t.Fatalf("cancel_by_tag did not cancel both ops receipts: %v", canceled)
		}
		if got[other] {
			t.Fatalf("cancel_by_tag canceled the other-tag receipt: %v", canceled)
		}

		// State checks: both ops receipts canceled, the other-tag still pending.
		for _, id := range []string{ops1, ops2} {
			rc, _ := a.repos.Receipts.GetByID(context.Background(), id)
			if rc.State != "canceled" {
				t.Fatalf("ops receipt %s state=%q want canceled", id, rc.State)
			}
		}
		rc, _ := a.repos.Receipts.GetByID(context.Background(), other)
		if rc.State != "pending" {
			t.Fatalf("other-tag receipt state=%q want pending (must be untouched)", rc.State)
		}
	})

	// --- canceling a delivered receipt is an error -------------------------
	t.Run("delivered_cancel_errors", func(t *testing.T) {
		a, st, base := newCancelEnv(t)
		c, tok, userKey := cancelSetup(t, base)
		r := sendP2WithTag(t, c, base, tok, userKey, "alert")
		// Force the receipt into the delivered state (todo 23 owns the real
		// pending->delivered transition; here we simulate it for the error path).
		setReceiptState(t, st, r, "delivered")

		status, _, body, raw := postCancel(t, c, base, r, tok)
		if status != http.StatusConflict {
			t.Fatalf("delivered cancel status=%d want 409; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
		errs, _ := body["errors"].([]any)
		if len(errs) == 0 {
			t.Fatalf("want non-empty errors: %s", raw)
		}
		if req, _ := body["request"].(string); req == "" {
			t.Fatalf("missing request id: %s", raw)
		}
		// State unchanged.
		rc, _ := a.repos.Receipts.GetByID(context.Background(), r)
		if rc.State != "delivered" {
			t.Fatalf("delivered receipt state changed to %q (must be untouched)", rc.State)
		}
	})

	// --- canceling an acknowledged receipt is an error ---------------------
	t.Run("acknowledged_cancel_errors", func(t *testing.T) {
		_, st, base := newCancelEnv(t)
		c, tok, userKey := cancelSetup(t, base)
		r := sendP2WithTag(t, c, base, tok, userKey, "alert")
		setReceiptState(t, st, r, "acknowledged")

		status, _, body, raw := postCancel(t, c, base, r, tok)
		if status != http.StatusConflict {
			t.Fatalf("acknowledged cancel status=%d want 409; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
		errs, _ := body["errors"].([]any)
		if len(errs) == 0 {
			t.Fatalf("want non-empty errors: %s", raw)
		}
	})

	// --- invalid token -> 401 with the canonical error --------------------
	t.Run("invalid_token_401", func(t *testing.T) {
		_, _, base := newCancelEnv(t)
		c, _, _ := cancelSetup(t, base)
		status, _, body, raw := postCancel(t, c, base, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ")
		if status != http.StatusUnauthorized {
			t.Fatalf("invalid token cancel status=%d want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0", body["status"])
		}
		errs, _ := body["errors"].([]any)
		if len(errs) != 1 || errs[0] != "application token is invalid" {
			t.Fatalf("errors=%v want exactly [\"application token is invalid\"]: %s", errs, raw)
		}
	})

	// --- unknown receipt -> 404 (not found, no leakage) -------------------
	t.Run("unknown_receipt_404", func(t *testing.T) {
		_, _, base := newCancelEnv(t)
		c, tok, _ := cancelSetup(t, base)
		status, _, body, raw := postCancel(t, c, base, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", tok)
		if status != http.StatusNotFound {
			t.Fatalf("unknown receipt cancel status=%d want 404; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})
}
