package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

// expectedSounds is the authoritative 23-entry snapshot copied from the #sounds
// section of pushover.net/api (fetched 2026-08-03). It MUST stay byte-identical
// to pushoverSounds in sounds.go; the test fails if either side drifts.
var expectedSounds = map[string]string{
	"pushover":     "Pushover (default)",
	"bike":         "Bike",
	"bugle":        "Bugle",
	"cashregister": "Cash Register",
	"classical":    "Classical",
	"cosmic":       "Cosmic",
	"falling":      "Falling",
	"gamelan":      "Gamelan",
	"incoming":     "Incoming",
	"intermission": "Intermission",
	"magic":        "Magic",
	"mechanical":   "Mechanical",
	"pianobar":     "Piano Bar",
	"siren":        "Siren",
	"spacealarm":   "Space Alarm",
	"tugboat":      "Tug Boat",
	"alien":        "Alien Alarm (long)",
	"climb":        "Climb (long)",
	"persistent":   "Persistent (long)",
	"echo":         "Pushover Echo (long)",
	"updown":       "Up Down (long)",
	"vibrate":      "Vibrate Only",
	"none":         "None (silent)",
}

// getSounds hits GET /1/sounds.json?token=<tok> and returns (status, decoded
// body, raw body). The token is appended as a query parameter exactly as
// Pushover clients call the endpoint.
func getSounds(t *testing.T, c *http.Client, baseURL, tok string) (int, map[string]any, []byte) {
	t.Helper()
	u := baseURL + "/1/sounds.json"
	if tok != "" {
		u += "?token=" + url.QueryEscape(tok)
	}
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("sounds GET: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, raw
}

func TestSounds(t *testing.T) {
	t.Run("happy_23_entries_exact_snapshot", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "sounds@example.com")
		tok := createApp(t, c, base, "sound-app")

		status, body, raw := getSounds(t, c, base, tok)
		if status != http.StatusOK {
			t.Fatalf("status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("status field=%v want 1: %s", body["status"], raw)
		}
		sounds, ok := body["sounds"].(map[string]any)
		if !ok {
			t.Fatalf("sounds not a map: %s", raw)
		}
		if len(sounds) != 23 {
			t.Fatalf("sounds count=%d, want exactly 23: %+v", len(sounds), sounds)
		}
		// Compare as a string map; json numbers are irrelevant here since every
		// value is a display-name string.
		got := make(map[string]string, len(sounds))
		for k, v := range sounds {
			s, _ := v.(string)
			got[k] = s
		}
		if !reflect.DeepEqual(got, expectedSounds) {
			t.Fatalf("sounds snapshot mismatch:\n got=%v\nwant=%v", got, expectedSounds)
		}
		// The 23 keys from the spec, in the exact set.
		for _, key := range []string{
			"pushover", "bike", "bugle", "cashregister", "classical", "cosmic",
			"falling", "gamelan", "incoming", "intermission", "magic", "mechanical",
			"pianobar", "siren", "spacealarm", "tugboat", "alien", "climb",
			"persistent", "echo", "updown", "vibrate", "none",
		} {
			if _, ok := sounds[key]; !ok {
				t.Fatalf("missing required sound key %q: %s", key, raw)
			}
		}
		// Guard against the in-code catalog and the test snapshot drifting
		// apart; both must name the same 23 display labels.
		if !reflect.DeepEqual(pushoverSounds, expectedSounds) {
			t.Fatalf("pushoverSounds drifted from expectedSounds test snapshot")
		}
		// Silence unused-var lint for the *Accounts handle in the pure-GET case.
		_ = a
	})

	t.Run("empty_token_400_status0", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		status, body, raw := getSounds(t, c, base, "")
		if status != http.StatusBadRequest {
			t.Fatalf("empty token status=%d want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	t.Run("invalid_token_400_status0", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		// Well-formed 30-char token that does not exist.
		status, body, raw := getSounds(t, c, base, "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ")
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

	t.Run("malformed_token_400_without_db_lookup", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		// A malformed token is rejected by the regex in ValidateAppToken
		// before any database query.
		status, body, _ := getSounds(t, c, base, "not-a-real-token")
		if status != http.StatusBadRequest {
			t.Fatalf("malformed token status=%d want 400", status)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0", body["status"])
		}
	})
}
