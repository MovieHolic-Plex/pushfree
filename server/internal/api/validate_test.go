package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"testing"

	"github.com/pushfree/pushfree/internal/store"
)

// postValidate POSTs a urlencoded token+user to /1/users/validate.json (the
// shape Pushover clients send) and returns (status, decoded body, raw body).
func postValidate(t *testing.T, c *http.Client, baseURL, tok, userKey string) (int, map[string]any, []byte) {
	t.Helper()
	form := url.Values{}
	if tok != "" {
		form.Set("token", tok)
	}
	if userKey != "" {
		form.Set("user", userKey)
	}
	resp, err := c.PostForm(baseURL+"/1/users/validate.json", form)
	if err != nil {
		t.Fatalf("validate POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, raw
}

// createDeviceFor inserts a registered device for userID directly through the
// store, mirroring what /1/devices/login.json (todo 13) persists. It returns
// nothing; the device becomes visible to validate.json's devices[] list.
func createDeviceFor(t *testing.T, a *Accounts, userID int64, deviceID, name string) {
	t.Helper()
	if _, err := a.repos.Devices.Create(context.Background(), &store.Device{
		UserID:     userID,
		DeviceID:   deviceID,
		SecretHash: "deadbeef", // validate.json never checks the secret
		Name:       name,
	}); err != nil {
		t.Fatalf("create device %q: %v", name, err)
	}
}

// userKeyFor looks up a registered user's user_key by email, for posting to
// validate.json alongside an app token.
func userKeyFor(t *testing.T, a *Accounts, email string) string {
	t.Helper()
	u, err := a.repos.Users.GetByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("get user %q: %v", email, err)
	}
	return u.UserKey
}

// deviceNames decodes the devices []any field into a sorted []string for
// order-independent comparison against the expected names.
func deviceNames(t *testing.T, body map[string]any, raw []byte) []string {
	t.Helper()
	devs, ok := body["devices"].([]any)
	if !ok {
		t.Fatalf("devices not a JSON array: %s", raw)
	}
	out := make([]string, 0, len(devs))
	for _, d := range devs {
		s, _ := d.(string)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestValidate(t *testing.T) {
	t.Run("happy_devices_and_licenses", func(t *testing.T) {
		// A user with two registered devices validates successfully; the
		// devices[] field lists exactly those names and licenses is [].
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "valid@example.com")
		tok := createApp(t, c, base, "validate-app")
		owner := sessionUserID(t, a, "valid@example.com")
		userKey := userKeyFor(t, a, "valid@example.com")

		createDeviceFor(t, a, owner, "dev-a", "phone")
		createDeviceFor(t, a, owner, "dev-b", "laptop")

		status, body, raw := postValidate(t, c, base, tok, userKey)
		if status != http.StatusOK {
			t.Fatalf("status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("status field=%v want 1: %s", body["status"], raw)
		}
		got := deviceNames(t, body, raw)
		want := []string{"laptop", "phone"} // sorted
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("devices=%v, want %v: %s", got, want, raw)
		}
		// licenses is always an empty array (pushfree has no paid licenses).
		lic, ok := body["licenses"].([]any)
		if !ok {
			t.Fatalf("licenses not a JSON array: %s", raw)
		}
		if len(lic) != 0 {
			t.Fatalf("licenses=%v, want empty array", lic)
		}
	})

	t.Run("no_devices_empty_array", func(t *testing.T) {
		// A valid user with zero devices returns devices: [] (not null),
		// which is the JSON a client parses as an empty list.
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "nodev@example.com")
		tok := createApp(t, c, base, "no-dev-app")
		userKey := userKeyFor(t, a, "nodev@example.com")

		status, body, raw := postValidate(t, c, base, tok, userKey)
		if status != http.StatusOK {
			t.Fatalf("status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("status field=%v want 1: %s", body["status"], raw)
		}
		devs, ok := body["devices"].([]any)
		if !ok {
			t.Fatalf("devices not a JSON array: %s", raw)
		}
		if len(devs) != 0 {
			t.Fatalf("devices=%v, want empty array", devs)
		}
	})

	t.Run("wrong_user_status0", func(t *testing.T) {
		// token A + user B's key: the user does not belong to the token's
		// owner, so the response is {"status":0,"errors":["user key is invalid"]}.
		a, base := newAppsTestServer(t)
		cOwner := newClient(t)
		registerLogin(t, cOwner, base, "owner@example.com")
		tok := createApp(t, cOwner, base, "owner-app")

		cOther := newClient(t)
		register(t, cOther, base, "other@example.com", "password1")
		otherKey := userKeyFor(t, a, "other@example.com")

		status, body, raw := postValidate(t, cOwner, base, tok, otherKey)
		if status < 400 || status >= 500 {
			t.Fatalf("wrong user status=%d, want 4xx; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
		errs, _ := body["errors"].([]any)
		if len(errs) != 1 || errs[0] != "user key is invalid" {
			t.Fatalf("errors=%v, want [\"user key is invalid\"]; raw=%s", errs, raw)
		}
	})

	t.Run("unknown_user_key_status0", func(t *testing.T) {
		// A well-formed 30-char user key that does not exist at all is the
		// same "user key is invalid" failure as a cross-user attempt (no
		// enumeration).
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "unk@example.com")
		tok := createApp(t, c, base, "unk-app")

		status, body, raw := postValidate(t, c, base, tok, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		if status < 400 || status >= 500 {
			t.Fatalf("unknown user status=%d, want 4xx; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	t.Run("invalid_token_status0", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "tok@example.com")

		// Unknown but well-formed 30-char token with any user value: the token
		// gate fires first and returns the canonical invalid-token body.
		status, body, raw := postValidate(t, c, base, "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ", "x")
		if status != http.StatusBadRequest {
			t.Fatalf("invalid token status=%d want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
		errs, _ := body["errors"].([]any)
		if len(errs) != 1 || errs[0] != "application token is invalid" {
			t.Fatalf("errors=%v, want [\"application token is invalid\"]; raw=%s", errs, raw)
		}
	})

	t.Run("devices_consistency_after_registration", func(t *testing.T) {
		// validate.json's devices[] must stay consistent with the store as
		// devices are registered: initially empty, then reflects each newly
		// added device in creation (id) order.
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "consistency@example.com")
		tok := createApp(t, c, base, "consistency-app")
		owner := sessionUserID(t, a, "consistency@example.com")
		userKey := userKeyFor(t, a, "consistency@example.com")

		// Initially no devices.
		_, body, _ := postValidate(t, c, base, tok, userKey)
		if len(deviceNames(t, body, nil)) != 0 {
			t.Fatalf("expected no devices before registration")
		}

		createDeviceFor(t, a, owner, "c1", "tablet")
		_, body, _ = postValidate(t, c, base, tok, userKey)
		if got := deviceNames(t, body, nil); len(got) != 1 || got[0] != "tablet" {
			t.Fatalf("after 1 device, devices=%v want [tablet]", got)
		}

		createDeviceFor(t, a, owner, "c2", "watch")
		_, body, _ = postValidate(t, c, base, tok, userKey)
		if got := deviceNames(t, body, nil); len(got) != 2 {
			t.Fatalf("after 2 devices, devices=%v want 2", got)
		}
	})

	t.Run("limits_headers_present", func(t *testing.T) {
		// /1/* responses carry the X-Limit-App-* convention via limitWrap.
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "lim@example.com")
		tok := createApp(t, c, base, "lim-app")
		userKey := userKeyFor(t, a, "lim@example.com")

		form := url.Values{}
		form.Set("token", tok)
		form.Set("user", userKey)
		resp, err := c.PostForm(base+"/1/users/validate.json", form)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Limit-App-Limit"); got != "10000" {
			t.Fatalf("X-Limit-App-Limit=%q want 10000", got)
		}
	})
}
