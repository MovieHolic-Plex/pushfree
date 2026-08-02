package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/pushfree/pushfree/internal/store/sqlite"
)

// newTestServer opens a fresh temp-file SQLite store and serves the accounts
// routes over httptest so cookies round-trip exactly as in production. Returns
// the server base URL.
func newTestServer(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pushfree-test.db")
	st, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	a := New(st.Repos(), []byte("unit-test-secret"), 0, nil)
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// newClient returns an http.Client that stores Set-Cookie in a jar, so a login
// response cookie is sent on subsequent requests automatically.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar}
}

func doJSON(t *testing.T, c *http.Client, method, url string, body any) (int, map[string]any, []byte) {
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
	return resp.StatusCode, decoded, raw
}

// register creates a user and returns its user_key, failing the test on any
// non-201 response.
func register(t *testing.T, c *http.Client, baseURL, email, password string) string {
	t.Helper()
	status, body, raw := doJSON(t, c, http.MethodPost, baseURL+"/v1/accounts", map[string]string{
		"email":    email,
		"password": password,
	})
	if status != http.StatusCreated {
		t.Fatalf("register %q: status=%d body=%s", email, status, raw)
	}
	if body["status"] != float64(1) {
		t.Fatalf("register status field = %v, want 1", body["status"])
	}
	key, _ := body["user_key"].(string)
	if key == "" {
		t.Fatalf("register returned empty user_key: %s", raw)
	}
	return key
}

// login authenticates and fails the test unless the response is 200 status:1.
func login(t *testing.T, c *http.Client, baseURL, email, password string) {
	t.Helper()
	status, body, raw := doJSON(t, c, http.MethodPost, baseURL+"/v1/accounts/login", map[string]string{
		"email": email, "password": password,
	})
	if status != http.StatusOK || body["status"] != float64(1) {
		t.Fatalf("login %q: status=%d body=%s", email, status, raw)
	}
}

func TestAccounts(t *testing.T) {
	t.Run("user_key_format", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		key := register(t, c, base, "alice@example.com", "password1")
		if !userKeyRe.MatchString(key) {
			t.Fatalf("user_key %q does not match ^[A-Za-z0-9]{30}$", key)
		}
	})

	t.Run("duplicate_email_409", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		register(t, c, base, "dup@example.com", "password1")
		status, body, raw := doJSON(t, c, http.MethodPost, base+"/v1/accounts", map[string]string{
			"email": "dup@example.com", "password": "password2",
		})
		if status != http.StatusConflict {
			t.Fatalf("duplicate email status = %d, want 409; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("duplicate status field = %v, want 0", body["status"])
		}
		if errs, _ := body["errors"].([]any); len(errs) == 0 {
			t.Fatalf("duplicate email must return errors array: %s", raw)
		}
	})

	t.Run("login_session_and_me", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		key := register(t, c, base, "sess@example.com", "password1")
		login(t, c, base, "sess@example.com", "password1")

		status, body, raw := doJSON(t, c, http.MethodGet, base+"/v1/accounts/me", nil)
		if status != http.StatusOK {
			t.Fatalf("me status = %d, want 200; body=%s", status, raw)
		}
		if body["email"] != "sess@example.com" {
			t.Fatalf("me email = %v", body["email"])
		}
		if body["user_key"] != key {
			t.Fatalf("me user_key = %v, want %q", body["user_key"], key)
		}
		qh, ok := body["quiet_hours"].(map[string]any)
		if !ok {
			t.Fatalf("me missing quiet_hours: %s", raw)
		}
		if qh["tz"] != "UTC" {
			t.Fatalf("default tz = %v, want UTC", qh["tz"])
		}
	})

	t.Run("first_user_admin_second_user", func(t *testing.T) {
		base := newTestServer(t)
		c1 := newClient(t)
		register(t, c1, base, "first@example.com", "password1")
		login(t, c1, base, "first@example.com", "password1")
		_, body, raw := doJSON(t, c1, http.MethodGet, base+"/v1/accounts/me", nil)
		if body["role"] != "admin" {
			t.Fatalf("first user role = %v, want admin; body=%s", body["role"], raw)
		}

		c2 := newClient(t)
		register(t, c2, base, "second@example.com", "password2")
		login(t, c2, base, "second@example.com", "password2")
		_, body, _ = doJSON(t, c2, http.MethodGet, base+"/v1/accounts/me", nil)
		if body["role"] != "user" {
			t.Fatalf("second user role = %v, want user", body["role"])
		}
	})

	t.Run("quiet_hours_stored_retrieved", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		register(t, c, base, "qh@example.com", "password1")
		login(t, c, base, "qh@example.com", "password1")

		status, _, raw := doJSON(t, c, http.MethodPut, base+"/v1/accounts/quiet-hours", map[string]string{
			"quiet_start": "22:00",
			"quiet_end":   "07:00",
			"tz":          "America/New_York",
		})
		if status != http.StatusOK {
			t.Fatalf("quiet-hours PUT status = %d, want 200; body=%s", status, raw)
		}

		_, body, _ := doJSON(t, c, http.MethodGet, base+"/v1/accounts/me", nil)
		qh, ok := body["quiet_hours"].(map[string]any)
		if !ok {
			t.Fatalf("me missing quiet_hours: %s", raw)
		}
		if qh["start"] != "22:00" || qh["end"] != "07:00" || qh["tz"] != "America/New_York" {
			t.Fatalf("quiet-hours not persisted: %+v", qh)
		}
	})

	t.Run("password_too_short_400", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		status, body, raw := doJSON(t, c, http.MethodPost, base+"/v1/accounts", map[string]string{
			"email": "short@example.com", "password": "only7", // 5 chars
		})
		if status != http.StatusBadRequest {
			t.Fatalf("short password status = %d, want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field = %v, want 0", body["status"])
		}
		errs, ok := body["errors"].([]any)
		if !ok || len(errs) == 0 {
			t.Fatalf("expected non-empty errors array, got: %s", raw)
		}
	})

	t.Run("seven_char_password_400", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		status, _, raw := doJSON(t, c, http.MethodPost, base+"/v1/accounts", map[string]string{
			"email": "seven@example.com", "password": "1234567", // exactly 7
		})
		if status != http.StatusBadRequest {
			t.Fatalf("7-char password status = %d, want 400; body=%s", status, raw)
		}
	})

	t.Run("wrong_login_401", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		register(t, c, base, "wrong@example.com", "password1")
		status, body, raw := doJSON(t, c, http.MethodPost, base+"/v1/accounts/login", map[string]string{
			"email": "wrong@example.com", "password": "not-the-password",
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("wrong login status = %d, want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field = %v, want 0", body["status"])
		}
	})

	t.Run("bad_tz_400", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		register(t, c, base, "tz@example.com", "password1")
		login(t, c, base, "tz@example.com", "password1")
		status, body, raw := doJSON(t, c, http.MethodPut, base+"/v1/accounts/quiet-hours", map[string]string{
			"quiet_start": "22:00",
			"quiet_end":   "07:00",
			"tz":          "Not/A/Zone",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("bad tz status = %d, want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field = %v, want 0", body["status"])
		}
		if errs, _ := body["errors"].([]any); len(errs) == 0 {
			t.Fatalf("bad tz must return errors array: %s", raw)
		}
	})

	t.Run("me_unauthenticated_401", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t) // no session
		status, body, raw := doJSON(t, c, http.MethodGet, base+"/v1/accounts/me", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("unauth me status = %d, want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field = %v, want 0", body["status"])
		}
	})

	t.Run("tampered_cookie_401", func(t *testing.T) {
		base := newTestServer(t)
		c := newClient(t)
		register(t, c, base, "tamper@example.com", "password1")
		login(t, c, base, "tamper@example.com", "password1")

		// Overwrite the valid cookie with a forged value carrying a far-future
		// expiry but an invalid signature. The client must reject it.
		origin, _ := url.Parse(base)
		c.Jar.SetCookies(origin, []*http.Cookie{
			{Name: sessionCookieName, Value: "1:9999999999.invalid-signature"},
		})
		status, _, _ := doJSON(t, c, http.MethodGet, base+"/v1/accounts/me", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("tampered cookie status = %d, want 401", status)
		}
	})
}
