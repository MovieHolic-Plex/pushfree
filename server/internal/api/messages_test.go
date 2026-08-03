package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/pushfree/pushfree/internal/store"
)

// stringsOf builds an n-rune ASCII string (runes == bytes for ASCII), for the
// boundary tests at 1024/1025, 250/251, 512/513, 100/101.
func stringsOf(n int) string { return strings.Repeat("a", n) }

// postMessages posts a urlencoded form to /1/messages.json and returns the
// decoded envelope, raw body, status, and response headers.
func postMessages(t *testing.T, c *http.Client, baseURL string, vals url.Values) (int, http.Header, map[string]any, []byte) {
	t.Helper()
	resp, err := c.PostForm(baseURL+"/1/messages.json", vals)
	if err != nil {
		t.Fatalf("post messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, resp.Header, decoded, raw
}

// postMessagesMultipart posts a multipart body to /1/messages.json carrying
// the given form fields and an optional attachment file part named "attachment".
func postMessagesMultipart(t *testing.T, c *http.Client, baseURL string, fields map[string]string, fileName string, fileData []byte) (int, http.Header, map[string]any, []byte) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			t.Fatalf("write field %q: %v", k, err)
		}
	}
	if fileData != nil {
		part, err := writer.CreateFormFile("attachment", fileName)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := part.Write(fileData); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/1/messages.json", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do multipart: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, resp.Header, decoded, raw
}

// requireEnvelope asserts the body is a {status:0, errors:[...], request:...}
// error envelope with a non-empty errors array and a UUID-shaped request id.
func requireEnvelope(t *testing.T, status int, body map[string]any, raw []byte, wantStatus int) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status=%d want %d; body=%s", status, wantStatus, raw)
	}
	if body["status"] != float64(0) {
		t.Fatalf("status field=%v want 0: %s", body["status"], raw)
	}
	errs, ok := body["errors"].([]any)
	if !ok || len(errs) == 0 {
		t.Fatalf("want non-empty errors array, got: %s", raw)
	}
	req, _ := body["request"].(string)
	if req == "" {
		t.Fatalf("want non-empty request id: %s", raw)
	}
}

// ingesterUser sets up one registered+logged-in user with an app token and
// returns (token, userKey, accounts) for direct store inspection.
func ingesterUser(t *testing.T) (*Accounts, string, string, string) {
	t.Helper()
	a, base := newAppsTestServer(t)
	c := newClient(t)
	userKey := register(t, c, base, "ingest@example.com", "password1")
	login(t, c, base, "ingest@example.com", "password1")
	tok := createApp(t, c, base, "monitoring")
	return a, base, tok, userKey
}

func TestIngest(t *testing.T) {
	// --- happy path: 200 status:1 + request uuid + X-Limit headers ---------
	t.Run("happy_200_status_request_and_limit_headers", func(t *testing.T) {
		a, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, hdr, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"hello world"},
		})
		if status != http.StatusOK {
			t.Fatalf("status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("status field=%v want 1: %s", body["status"], raw)
		}
		req, _ := body["request"].(string)
		if req == "" {
			t.Fatalf("missing request id: %s", raw)
		}
		// request is a UUID (contains hyphens, 36 chars).
		if len(req) != 36 || !strings.Contains(req, "-") {
			t.Fatalf("request %q is not a uuid", req)
		}
		// No receipt for non-emergency priority.
		if _, ok := body["receipt"]; ok {
			t.Fatalf("non-p2 response must not carry receipt: %s", raw)
		}
		// X-Limit headers present, remaining reflects the one accepted send.
		assertLimitHeaders(t, hdr, monthlyLimit-1)

		// One message row was fanned out to the recipient (self).
		uid := sessionUserID(t, a, "ingest@example.com")
		msgs, err := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("want 1 message row, got %d", len(msgs))
		}
	})

	// --- missing required fields -> 400 errors array -----------------------
	t.Run("missing_token_user_message_400", func(t *testing.T) {
		_, base, _, _ := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
		errs, _ := body["errors"].([]any)
		joined := strings.ToLower(strings.Join(toStrings(errs), " "))
		for _, want := range []string{"token", "user", "message"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("errors %q missing %q: %s", joined, want, raw)
			}
		}
	})

	// --- message boundary: 1024 ok / 1025 -> 400 ---------------------------
	t.Run("message_boundary_1024_ok_1025_400", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)

		// exactly 1024 runes -> accepted (UTF-8 rune count, not bytes).
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {stringsOf(1024)},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("1024-rune message: status=%d body=%s", status, raw)
		}

		// 1025 runes -> 400. Verify the limit is on RUNES: a 1025-rune string
		// that is only 1024 bytes if measured by bytes is still rejected.
		status, _, body, raw = postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {stringsOf(1025)},
		})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
	})

	// --- multibyte message: rune count governs, not byte count ------------
	t.Run("message_multibyte_rune_count", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		// 1024 copies of a 3-byte rune => 3072 bytes but 1024 runes -> ok.
		msg := strings.Repeat("€", 1024)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {msg},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("1024-rune multibyte message should be accepted: status=%d body=%s", status, raw)
		}
	})

	// --- title / url / url_title boundaries -------------------------------
	t.Run("title_url_urltitle_boundaries", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)

		// All at their exact maxima -> accepted.
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
			"title":     {stringsOf(250)},
			"url":       {stringsOf(512)},
			"url_title": {stringsOf(100)},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("at-limit fields should be accepted: status=%d body=%s", status, raw)
		}

		for _, tc := range []struct {
			field string
			n     int
		}{
			{"title", 251}, {"url", 513}, {"url_title", 101},
		} {
			status, _, body, raw := postMessages(t, c, base, url.Values{
				"token": {tok}, "user": {userKey}, "message": {"m"},
				tc.field: {stringsOf(tc.n)},
			})
			requireEnvelope(t, status, body, raw, http.StatusBadRequest)
		}
	})

	// --- priority out of range -> 400; -2..2 accepted ----------------------
	t.Run("priority_range", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		for _, p := range []string{"-2", "-1", "0", "1", "2"} {
			status, _, body, raw := postMessages(t, c, base, url.Values{
				"token": {tok}, "user": {userKey}, "message": {"m"}, "priority": {p},
			})
			if status != http.StatusOK || body["status"] != float64(1) {
				t.Fatalf("priority %s should be accepted: status=%d body=%s", p, status, raw)
			}
		}
		for _, p := range []string{"-3", "3", "99"} {
			status, _, body, raw := postMessages(t, c, base, url.Values{
				"token": {tok}, "user": {userKey}, "message": {"m"}, "priority": {p},
			})
			requireEnvelope(t, status, body, raw, http.StatusBadRequest)
		}
	})

	// --- device: 25 ok / 26 -> 400; bad charset -> 400 --------------------
	t.Run("device_boundaries", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		// 25-char device name accepted; comma list accepted.
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
			"device": {stringsOf(25) + ",phone-1"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("valid device list should be accepted: status=%d body=%s", status, raw)
		}
		// 26-char device name -> 400.
		status, _, body, raw = postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
			"device": {stringsOf(26)},
		})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
		// bad charset -> 400.
		status, _, body, raw = postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
			"device": {"bad device!"},
		})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
	})

	// --- html=1 + monospace=1 -> 400 (mutual exclusion) -------------------
	t.Run("html_monospace_mutual_exclusion", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
			"html": {"1"}, "monospace": {"1"},
		})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
		// Either alone is fine.
		for _, vals := range []url.Values{
			{"token": {tok}, "user": {userKey}, "message": {"m"}, "html": {"1"}},
			{"token": {tok}, "user": {userKey}, "message": {"m"}, "monospace": {"1"}},
		} {
			status, _, body, raw := postMessages(t, c, base, vals)
			if status != http.StatusOK || body["status"] != float64(1) {
				t.Fatalf("single html/monospace should be accepted: status=%d body=%s", status, raw)
			}
		}
	})

	// --- ttl: negative -> 400; zero/positive accepted ---------------------
	t.Run("ttl_negative_400", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"}, "ttl": {"-1"},
		})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
		for _, ttl := range []string{"0", "60", "3600"} {
			status, _, body, raw := postMessages(t, c, base, url.Values{
				"token": {tok}, "user": {userKey}, "message": {"m"}, "ttl": {ttl},
			})
			if status != http.StatusOK || body["status"] != float64(1) {
				t.Fatalf("ttl %s should be accepted: status=%d body=%s", ttl, status, raw)
			}
		}
	})

	// --- attachment: exactly 5242880 ok / 5242881 -> 400 (multipart) -------
	t.Run("attachment_multipart_size_boundary", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		// exactly at the cap -> accepted.
		status, _, body, raw := postMessagesMultipart(t, c, base, map[string]string{
			"token": tok, "user": userKey, "message": "see attached",
		}, "f.bin", bytes.Repeat([]byte{0}, 5242880))
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("max-size attachment should be accepted: status=%d body=%s", status, raw)
		}
		// one byte over -> 400.
		status, _, body, raw = postMessagesMultipart(t, c, base, map[string]string{
			"token": tok, "user": userKey, "message": "see attached",
		}, "f.bin", bytes.Repeat([]byte{0}, 5242881))
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
	})

	// --- attachment: base64 form over the cap -> 400 -----------------------
	t.Run("attachment_base64_size_boundary", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		over := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, 5242881))
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
			"attachment_base64": {over}, "attachment_type": {"image/png"},
		})
		requireEnvelope(t, status, body, raw, http.StatusBadRequest)
	})

	// --- tags + callback accepted and stored ------------------------------
	t.Run("tags_callback_stored", func(t *testing.T) {
		a, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
			"tags": {"ops,incident"}, "callback": {"https://example.com/cb"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("tags+callback should be accepted: status=%d body=%s", status, raw)
		}
		uid := sessionUserID(t, a, "ingest@example.com")
		msgs, err := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		var sd store.Send
		sd, err = a.repos.Sends.GetByID(context.Background(), msgs[0].SendID)
		if err != nil {
			t.Fatalf("get send: %v", err)
		}
		if sd.Tag != "ops,incident" {
			t.Fatalf("tag not stored: %q", sd.Tag)
		}
		if sd.CallbackURL != "https://example.com/cb" {
			t.Fatalf("callback_url not stored: %q", sd.CallbackURL)
		}
	})

	// --- encrypted=1 accepted and stored ----------------------------------
	t.Run("encrypted_flag_stored", func(t *testing.T) {
		a, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"}, "encrypted": {"1"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("encrypted=1 should be accepted: status=%d body=%s", status, raw)
		}
		uid := sessionUserID(t, a, "ingest@example.com")
		msgs, _ := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
		sd, err := a.repos.Sends.GetByID(context.Background(), msgs[0].SendID)
		if err != nil {
			t.Fatalf("get send: %v", err)
		}
		if !sd.Encrypted {
			t.Fatalf("encrypted flag not stored")
		}
	})

	// --- sound: unknown/custom value accepted and stored ------------------
	t.Run("sound_unknown_stored", func(t *testing.T) {
		a, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"}, "sound": {"totally-made-up"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("unknown sound should be accepted: status=%d body=%s", status, raw)
		}
		uid := sessionUserID(t, a, "ingest@example.com")
		msgs, _ := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
		sd, _ := a.repos.Sends.GetByID(context.Background(), msgs[0].SendID)
		if sd.Sound != "totally-made-up" {
			t.Fatalf("sound not stored: %q", sd.Sound)
		}
	})

	// --- priority=2: response carries a 30-char receipt + receipt row ------
	t.Run("priority2_receipt", func(t *testing.T) {
		a, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"emergency"},
			"priority": {"2"}, "tags": {"alert"},
		})
		if status != http.StatusOK {
			t.Fatalf("p2 status=%d want 200; body=%s", status, raw)
		}
		if body["status"] != float64(1) {
			t.Fatalf("status field=%v want 1: %s", body["status"], raw)
		}
		receipt, _ := body["receipt"].(string)
		if !userKeyRe.MatchString(receipt) || len(receipt) != 30 {
			t.Fatalf("receipt %q is not 30-char [A-Za-z0-9]: %s", receipt, raw)
		}
		// A receipt row exists in initial pending state, tagged for cancel_by_tag.
		rc, err := a.repos.Receipts.GetByID(context.Background(), receipt)
		if err != nil {
			t.Fatalf("get receipt: %v", err)
		}
		if rc.State != "pending" {
			t.Fatalf("receipt state=%q want pending", rc.State)
		}
		if rc.Tag != "alert" {
			t.Fatalf("receipt tag=%q want alert", rc.Tag)
		}
	})

	// --- invalid token -> 401 with exact error + request id ---------------
	t.Run("invalid_token_401", func(t *testing.T) {
		_, base, _, _ := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {"ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"}, "user": {"x"}, "message": {"m"},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("invalid token status=%d want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0", body["status"])
		}
		errs, _ := body["errors"].([]any)
		if len(errs) != 1 || errs[0] != "application token is invalid" {
			t.Fatalf("errors=%v want exactly [\"application token is invalid\"]: %s", errs, raw)
		}
		if req, _ := body["request"].(string); req == "" {
			t.Fatalf("401 must carry request id: %s", raw)
		}
	})

	// --- revoked token -> 401 ---------------------------------------------
	t.Run("revoked_token_401", func(t *testing.T) {
		_, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		// The DELETE /v1/apps route is session-auth: establish the session on
		// this client before revoking (ingesterUser uses its own client).
		login(t, c, base, "ingest@example.com", "password1")
		req, _ := http.NewRequest(http.MethodDelete, base+"/v1/apps/"+tok, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("delete app: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("delete app status=%d want 200 (revoke must succeed)", resp.StatusCode)
		}
		resp.Body.Close()
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {userKey}, "message": {"m"},
		})
		if status != http.StatusUnauthorized {
			t.Fatalf("revoked token status=%d want 401; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0", body["status"])
		}
	})

	// --- recipient is another user: fan-out succeeds (todo 9) -------------
	t.Run("user_other_fanout_succeeds", func(t *testing.T) {
		a, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		// Create a second user; sending to the second user's key from the
		// first user's app token is allowed in todo 9 (multi-user fan-out).
		otherKey := register(t, newClient(t), base, "other@example.com", "password1")
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {otherKey}, "message": {"m"},
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("fan-out to another user should succeed: status=%d body=%s", status, raw)
		}
		// A message row was fanned out to the OTHER user (not the sender).
		otherID := sessionUserID(t, a, "other@example.com")
		msgs, err := a.repos.Messages.ListSince(context.Background(), otherID, 0, 100)
		if err != nil {
			t.Fatalf("list messages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("want 1 message for other user, got %d", len(msgs))
		}
	})

	// --- unknown user/group key -> 404 (todo 9: not found) ----------------
	t.Run("unknown_key_404", func(t *testing.T) {
		_, base, tok, _ := ingesterUser(t)
		c := newClient(t)
		// Well-formed 30-char but not a real user_key or group_key.
		status, _, body, raw := postMessages(t, c, base, url.Values{
			"token": {tok}, "user": {"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}, "message": {"m"},
		})
		requireEnvelope(t, status, body, raw, http.StatusNotFound)
	})

	// --- attachment persisted via ingest ----------------------------------
	t.Run("attachment_stored", func(t *testing.T) {
		a, base, tok, userKey := ingesterUser(t)
		c := newClient(t)
		status, _, body, raw := postMessagesMultipart(t, c, base, map[string]string{
			"token": tok, "user": userKey, "message": "pic",
		}, "note.txt", []byte("hello-bytes"))
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("attachment send should be accepted: status=%d body=%s", status, raw)
		}
		uid := sessionUserID(t, a, "ingest@example.com")
		msgs, _ := a.repos.Messages.ListSince(context.Background(), uid, 0, 100)
		att, err := a.repos.Attachments.GetBySendID(context.Background(), msgs[0].SendID)
		if err != nil {
			t.Fatalf("get attachment: %v", err)
		}
		if string(att.Data) != "hello-bytes" {
			t.Fatalf("attachment data mismatch: %q", att.Data)
		}
	})
}

// toStrings coerces a []any (from JSON decode) to []string.
func toStrings(v []any) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i], _ = s.(string)
	}
	return out
}
