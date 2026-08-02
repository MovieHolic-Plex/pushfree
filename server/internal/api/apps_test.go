package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/store/sqlite"
)

// newAppsTestServer is like newTestServer but also returns the wired *Accounts
// so tests can drive unexported helpers (quotaSnapshot, ValidateAppToken) and
// the store bundle directly. Routes (accounts + /v1/apps) are registered
// exactly as production wires them.
func newAppsTestServer(t *testing.T) (*Accounts, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pushfree-apps-test.db")
	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := New(st.Repos(), []byte("apps-test-secret"), 0, nil)
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, srv.URL
}

// registerLogin is the happy-path setup shared by the app subtests.
func registerLogin(t *testing.T, c *http.Client, baseURL, email string) {
	t.Helper()
	register(t, c, baseURL, email, "password1")
	login(t, c, baseURL, email, "password1")
}

// doReq is doJSON that also returns response headers, where useful.
func doReq(t *testing.T, c *http.Client, method, url string, body any) (int, http.Header, map[string]any, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, resp.Header, decoded, raw
}

// sessionUserID looks up the logged-in user's store id, for direct quota
// manipulation and ValidateAppToken comparisons.
func sessionUserID(t *testing.T, a *Accounts, email string) int64 {
	t.Helper()
	u, err := a.repos.Users.GetByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("get user %q: %v", email, err)
	}
	return u.ID
}

// createApp triggers POST /v1/apps and returns the new token, failing on any
// non-success envelope.
func createApp(t *testing.T, c *http.Client, baseURL, name string) string {
	t.Helper()
	status, _, body, raw := doReq(t, c, http.MethodPost, baseURL+"/v1/apps", map[string]string{"name": name})
	if status != http.StatusCreated {
		t.Fatalf("create app %q: status=%d body=%s", name, status, raw)
	}
	if body["status"] != float64(1) {
		t.Fatalf("create app status field=%v, want 1: %s", body["status"], raw)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("create app returned empty token: %s", raw)
	}
	return tok
}

// assertLimitHeaders validates the three X-Limit-App-* headers a /1/* send
// response carries: fixed limit, remaining consistent with the counter, reset
// at the next UTC calendar-month boundary.
func assertLimitHeaders(t *testing.T, hdr http.Header, wantRemaining int) {
	t.Helper()
	if got := hdr.Get("X-Limit-App-Limit"); got != "10000" {
		t.Fatalf("X-Limit-App-Limit=%q want 10000", got)
	}
	if got := hdr.Get("X-Limit-App-Remaining"); got != strconv.Itoa(wantRemaining) {
		t.Fatalf("X-Limit-App-Remaining=%q want %d", got, wantRemaining)
	}
	reset, err := strconv.ParseInt(hdr.Get("X-Limit-App-Reset"), 10, 64)
	if err != nil {
		t.Fatalf("X-Limit-App-Reset not int: %q", hdr.Get("X-Limit-App-Reset"))
	}
	rt := time.Unix(reset, 0).UTC()
	if rt.Day() != 1 || rt.Hour() != 0 || rt.Minute() != 0 || rt.Second() != 0 {
		t.Fatalf("reset %v is not a month boundary (1st 00:00:00 UTC)", rt)
	}
	now := time.Now().UTC()
	if !rt.After(now) {
		t.Fatalf("reset %v not in the future (now %v)", rt, now)
	}
	if rt.Sub(now) > 32*24*time.Hour {
		t.Fatalf("reset %v is more than a month out (now %v)", rt, now)
	}
}

func TestApps(t *testing.T) {
	t.Run("create_token_format", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "creator@example.com")
		tok := createApp(t, c, base, "monitoring")
		// Acceptance: token matches ^[A-Za-z0-9]{30}$ (userKeyRe is that regex).
		if !userKeyRe.MatchString(tok) {
			t.Fatalf("token %q does not match ^[A-Za-z0-9]{30}$", tok)
		}
		if len(tok) != 30 {
			t.Fatalf("token length = %d, want 30", len(tok))
		}
	})

	t.Run("list_returns_tokens", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "lister@example.com")
		tok1 := createApp(t, c, base, "app-a")
		tok2 := createApp(t, c, base, "app-b")

		status, _, body, raw := doReq(t, c, http.MethodGet, base+"/v1/apps?all=1", nil)
		if status != http.StatusOK {
			t.Fatalf("list status=%d body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("list status field=%v, want 1", body["status"])
		}
		apps, ok := body["apps"].([]any)
		if !ok || len(apps) != 2 {
			t.Fatalf("want 2 apps, got %v: %s", body["apps"], raw)
		}
		have := map[string]string{}
		for _, ap := range apps {
			m, _ := ap.(map[string]any)
			have[m["token"].(string)] = m["name"].(string)
		}
		if have[tok1] != "app-a" || have[tok2] != "app-b" {
			t.Fatalf("list missing created tokens: %+v", have)
		}
	})

	t.Run("list_empty_when_none", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "empty@example.com")
		_, _, body, raw := doReq(t, c, http.MethodGet, base+"/v1/apps", nil)
		apps, _ := body["apps"].([]any)
		if len(apps) != 0 {
			t.Fatalf("want empty list, got %v: %s", apps, raw)
		}
	})

	t.Run("delete_revokes_and_validation_fails", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "revoker@example.com")
		tok := createApp(t, c, base, "to-revoke")
		owner := sessionUserID(t, a, "revoker@example.com")

		// Valid before revoke: resolves to the session user.
		if uid, err := a.ValidateAppToken(context.Background(), tok); err != nil || uid != owner {
			t.Fatalf("validate before revoke: uid=%d owner=%d err=%v", uid, owner, err)
		}

		// Revoke.
		status, _, body, raw := doReq(t, c, http.MethodDelete, base+"/v1/apps/"+tok, nil)
		if status != http.StatusOK {
			t.Fatalf("delete status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("delete status field=%v want 1: %s", body["status"], raw)
		}

		// No longer listed.
		_, _, listBody, _ := doReq(t, c, http.MethodGet, base+"/v1/apps", nil)
		for _, ap := range listBody["apps"].([]any) {
			if ap.(map[string]any)["token"] == tok {
				t.Fatalf("revoked token still listed")
			}
		}

		// Validation now fails with the sentinel (deliverable 3, tested directly).
		if _, err := a.ValidateAppToken(context.Background(), tok); !errors.Is(err, ErrInvalidAppToken) {
			t.Fatalf("validate after revoke err=%v, want ErrInvalidAppToken", err)
		}
	})

	t.Run("invalid_token_401_body_exact", func(t *testing.T) {
		// Simulates the send-path 401 (todo 8 will call WriteInvalidAppToken).
		rec := httptest.NewRecorder()
		WriteInvalidAppToken(rec)
		resp := rec.Result()
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401", resp.StatusCode)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode body: %v: %s", err, rec.Body.String())
		}
		if got["status"] != float64(0) {
			t.Fatalf("status=%v want 0", got["status"])
		}
		errs, _ := got["errors"].([]any)
		if len(errs) != 1 || errs[0] != "application token is invalid" {
			t.Fatalf("errors=%v, want exactly [\"application token is invalid\"]; raw=%s",
				errs, rec.Body.String())
		}
	})

	t.Run("validate_malformed_without_db", func(t *testing.T) {
		a, _ := newAppsTestServer(t)
		for _, bad := range []string{"", "short", "with space here!!", "X"} {
			if _, err := a.ValidateAppToken(context.Background(), bad); !errors.Is(err, ErrInvalidAppToken) {
				t.Fatalf("ValidateAppToken(%q) err=%v, want ErrInvalidAppToken", bad, err)
			}
		}
		// Unknown but well-formed 30-char token -> ErrInvalidAppToken.
		unknown := "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
		if _, err := a.ValidateAppToken(context.Background(), unknown); !errors.Is(err, ErrInvalidAppToken) {
			t.Fatalf("ValidateAppToken(unknown) err=%v, want ErrInvalidAppToken", err)
		}
	})

	t.Run("limit_headers_full_quota", func(t *testing.T) {
		// The X-Limit helper todo 8 attaches to /1/* send responses. With a
		// zero quota counter it reports the full limit.
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "limit@example.com")
		uid := sessionUserID(t, a, "limit@example.com")

		rec := httptest.NewRecorder()
		a.SetLimitHeaders(rec, uid)
		assertLimitHeaders(t, rec.Result().Header, monthlyLimit)
	})

	t.Run("limit_remaining_matches_quota", func(t *testing.T) {
		// Header values must stay consistent with the quota counter: after N
		// accepted sends, remaining == limit - N.
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "quota@example.com")
		uid := sessionUserID(t, a, "quota@example.com")

		period := quotaPeriod(time.Now())
		after, err := a.repos.Quota.Increment(context.Background(), uid, period, 5)
		if err != nil {
			t.Fatalf("increment: %v", err)
		}
		if after != 5 {
			t.Fatalf("count=%d want 5", after)
		}

		rec := httptest.NewRecorder()
		a.SetLimitHeaders(rec, uid)
		assertLimitHeaders(t, rec.Result().Header, monthlyLimit-5)
	})

	t.Run("limit_wrap_attachable_to_send_path", func(t *testing.T) {
		// limitWrap is the middleware form todo 8 wraps /1/* send routes with.
		// It resolves the user from the session cookie and sets the headers.
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "wrap@example.com")

		mux := http.NewServeMux()
		mux.HandleFunc("GET /1/ping", a.limitWrap(a.requireSession(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"status": 1})
		})))
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		// The session cookie (Path=/, host 127.0.0.1) carries across the
		// throwaway server's port because Go's cookiejar ignores port.
		resp, err := c.Get(srv.URL + "/1/ping")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ping status=%d", resp.StatusCode)
		}
		assertLimitHeaders(t, resp.Header, monthlyLimit)
	})

	t.Run("mark_send_accepted_clamps_at_zero", func(t *testing.T) {
		// Driving the quota above the limit must clamp remaining at 0, not go
		// negative. This is the contract todo 8 relies on for the success-path
		// header (and todo 10 for the 429 gate).
		a, _ := newAppsTestServer(t)
		ctx := context.Background()
		uid, err := a.repos.Users.CreateBootstrap(ctx, &store.User{
			Email:     "clamp@example.com",
			PassHash:  "x",
			UserKey:   "KKKKKKKKKKKKKKKKKKKKKKKKKKKKKK", // 30 chars
			QuietTZ:   "UTC",
			CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		period := quotaPeriod(time.Now())
		if _, err := a.repos.Quota.Increment(ctx, uid, period, int64(monthlyLimit+50)); err != nil {
			t.Fatalf("increment over limit: %v", err)
		}
		rem, err := a.MarkSendAccepted(ctx, uid)
		if err != nil {
			t.Fatalf("mark send accepted: %v", err)
		}
		if rem != 0 {
			t.Fatalf("remaining=%d want 0 after exceeding limit", rem)
		}
	})

	t.Run("create_requires_session_401", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t) // no session cookie
		status, _, body, raw := doReq(t, c, http.MethodPost, base+"/v1/apps", map[string]string{"name": "x"})
		if status != http.StatusUnauthorized {
			t.Fatalf("unauthenticated create status=%d want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0", body["status"])
		}
	})

	t.Run("delete_not_owned_404", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		cOwner := newClient(t)
		registerLogin(t, cOwner, base, "owner@example.com")
		tok := createApp(t, cOwner, base, "owned")

		cOther := newClient(t)
		registerLogin(t, cOther, base, "other@example.com")
		status, _, _, raw := doReq(t, cOther, http.MethodDelete, base+"/v1/apps/"+tok, nil)
		if status != http.StatusNotFound {
			t.Fatalf("cross-user delete status=%d want 404; body=%s", status, raw)
		}
		// Owner's token is still valid (not revoked by the foreign attempt).
		if _, err := a.ValidateAppToken(context.Background(), tok); err != nil {
			t.Fatalf("owner token revoked by cross-user attempt: %v", err)
		}
	})

	t.Run("create_missing_name_400", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "named@example.com")
		status, _, body, raw := doReq(t, c, http.MethodPost, base+"/v1/apps", map[string]string{"name": "   "})
		if status != http.StatusBadRequest {
			t.Fatalf("blank-name status=%d want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0", body["status"])
		}
	})
}
