package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/quota"
)

// getLimits GETs /1/apps/limits.json?token=tok and returns status, headers,
// decoded body, and the raw body bytes. Used for the limits.json and 429
// scenarios (the todo 10 manual-QA surface, automated).
func getLimits(t *testing.T, c *http.Client, baseURL, token string) (int, http.Header, map[string]any, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/1/apps/limits.json?token="+url.QueryEscape(token), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("get limits: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, resp.Header, decoded, raw
}

// TestQuotaLimitsJSON covers GET /1/apps/limits.json (todo 10): field set,
// consistency with the live counter, and the missing-token 400.
func TestQuotaLimitsJSON(t *testing.T) {
	t.Run("fields_consistent_with_counter", func(t *testing.T) {
		a, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		uid := sessionUserID(t, a, "ingest@example.com")

		// Fresh: count=0, remaining=limit, reset is a CT month boundary.
		status, hdr, body, raw := getLimits(t, c, base, tok)
		if status != http.StatusOK {
			t.Fatalf("limits status=%d body=%s", status, raw)
		}
		count, _ := body["count"].(float64)
		limit, _ := body["limit"].(float64)
		remaining, _ := body["remaining"].(float64)
		reset, _ := body["reset"].(float64)
		if int(limit) != monthlyLimit {
			t.Fatalf("limit=%v want %d: %s", limit, monthlyLimit, raw)
		}
		if int(count) != 0 || int(remaining) != monthlyLimit {
			t.Fatalf("fresh count=%v remaining=%v want 0/%d: %s", count, remaining, monthlyLimit, raw)
		}
		if int(count)+int(remaining) != int(limit) {
			t.Fatalf("inconsistent count(%v)+remaining(%v)!=limit(%v): %s", count, remaining, limit, raw)
		}
		// reset lands on a CT month boundary.
		rt := time.Unix(int64(reset), 0).In(quota.CentralTime)
		if rt.Day() != 1 || rt.Hour() != 0 || rt.Minute() != 0 || rt.Second() != 0 {
			t.Fatalf("reset %v is not 00:00:00 on the 1st in America/Chicago", rt)
		}
		// The route is wrapped by limitWrap, so the headers mirror the body.
		if hdr.Get("X-Limit-App-Remaining") != strconv.Itoa(monthlyLimit) {
			t.Fatalf("X-Limit-App-Remaining=%q want %d", hdr.Get("X-Limit-App-Remaining"), monthlyLimit)
		}

		// After one accepted send, count advances to 1 and remaining to limit-1.
		_, _, _, _ = postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKeyForSender(t, a, uid)}, "message": {"q"},
		})
		_, _, body2, raw2 := getLimits(t, c, base, tok)
		if int(body2["count"].(float64)) != 1 || int(body2["remaining"].(float64)) != monthlyLimit-1 {
			t.Fatalf("after 1 send count=%v remaining=%v want 1/%d: %s",
				body2["count"], body2["remaining"], monthlyLimit-1, raw2)
		}
	})

	t.Run("missing_token_400", func(t *testing.T) {
		_, base, _, _ := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := getLimits(t, c, base, "")
		if status != http.StatusBadRequest {
			t.Fatalf("missing-token status=%d want 400: %s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})
}

// TestQuotaOverLimit429 covers the failure scenario from manual QA: exhaust
// the monthly quota, then a further send returns 429 with the exact error
// envelope AND X-Limit-App-Remaining:0. Group partial-overflow is rejected as
// a whole (todo 9 consistency: charge per member).
func TestQuotaOverLimit429(t *testing.T) {
	t.Run("exhausted_then_429_remaining_zero", func(t *testing.T) {
		a, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		uid := sessionUserID(t, a, "ingest@example.com")
		userKey := userKeyForSender(t, a, uid)

		// Pre-charge the counter to the limit via the store (the live send
		// path would take 10000 calls). The next send is the 10001st.
		ctx := context.Background()
		if _, err := a.repos.Quota.Increment(ctx, uid, quotaPeriod(time.Now()), int64(monthlyLimit)); err != nil {
			t.Fatalf("pre-charge quota: %v", err)
		}

		status, hdr, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"one too many"},
		})
		if status != http.StatusTooManyRequests {
			t.Fatalf("over-limit status=%d want 429: %s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
		errs, _ := body["errors"].([]any)
		if len(errs) != 1 || errs[0] != "application reached monthly message limit" {
			t.Fatalf("errors=%v want exactly [application reached monthly message limit]: %s", errs, raw)
		}
		if hdr.Get("X-Limit-App-Remaining") != "0" {
			t.Fatalf("X-Limit-App-Remaining=%q want 0 on 429", hdr.Get("X-Limit-App-Remaining"))
		}
		// No send row should have been persisted (pre-write gate).
		if qc, _ := a.repos.Quota.Get(ctx, uid, quotaPeriod(time.Now())); qc.Count != int64(monthlyLimit) {
			t.Fatalf("counter mutated on rejected send: count=%d want %d", qc.Count, monthlyLimit)
		}
	})

	t.Run("group_partial_overflow_rejected_whole", func(t *testing.T) {
		// remaining == 1; a 2-recipient send is rejected as a whole, leaving
		// the counter untouched.
		a, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		uid := sessionUserID(t, a, "ingest@example.com")
		userKey := userKeyForSender(t, a, uid)
		ctx := context.Background()
		if _, err := a.repos.Quota.Increment(ctx, uid, quotaPeriod(time.Now()), int64(monthlyLimit-1)); err != nil {
			t.Fatalf("pre-charge: %v", err)
		}
		// Two recipients (the sender's own key twice -> 2 resolved IDs).
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey + "," + userKey}, "message": {"needs 2"},
		})
		if status != http.StatusTooManyRequests {
			t.Fatalf("partial-overflow status=%d want 429: %s", status, raw)
		}
		errs, _ := body["errors"].([]any)
		if len(errs) != 1 || errs[0] != "application reached monthly message limit" {
			t.Fatalf("errors=%v: %s", errs, raw)
		}
		if qc, _ := a.repos.Quota.Get(ctx, uid, quotaPeriod(time.Now())); qc.Count != int64(monthlyLimit-1) {
			t.Fatalf("counter mutated on rejected send: count=%d want %d", qc.Count, monthlyLimit-1)
		}
	})
}

// userKeyForSender returns the user_key for uid (the ingester), so the quota
// tests can send to the sender themselves without an extra registration.
func userKeyForSender(t *testing.T, a *Accounts, uid int64) string {
	t.Helper()
	u, err := a.repos.Users.GetByID(context.Background(), uid)
	if err != nil {
		t.Fatalf("get user %d: %v", uid, err)
	}
	return u.UserKey
}
