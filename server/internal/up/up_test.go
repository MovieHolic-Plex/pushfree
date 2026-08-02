package up_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/store/sqlite"
	"github.com/pushfree/pushfree/internal/up"
)

// testTime is the fixed instant used for seeded rows.
var testTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// authSecret keys both the (unused here) session signing and the sub
// derivation; it is fixed so DeriveSub can be recomputed in tests.
var authSecret = []byte("test-up-secret-do-not-hardcode-the-sub")

// idRe matches a 30-char [A-Za-z0-9] identifier (device_id / secret).
var idRe = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)

// subRe matches the 4-char derived subscription key.
var subRe = regexp.MustCompile(`^[A-Za-z0-9]{4}$`)

// stubResolver is a SessionUserResolver that always resolves to one user.
type stubResolver struct{ userID int64 }

func (s stubResolver) ResolveUserID(*http.Request) (int64, bool) { return s.userID, true }

// noResolver never resolves (no session).
type noResolver struct{}

func (noResolver) ResolveUserID(*http.Request) (int64, bool) { return 0, false }

// env wires a UP handler against a real temp-file SQLite store over httptest.
type env struct {
	t       *testing.T
	h       *up.Handler
	srv     *httptest.Server
	repos   store.Repos
	st      *sqlite.Store
	userID  int64
	appID   int64
	hmacKey []byte
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "up.db")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	repos := st.Repos()

	u := store.User{
		Email: "up@pushfree.local", PassHash: "x", Role: "user",
		UserKey: key30(1), QuietTZ: "UTC", CreatedAt: testTime,
	}
	uid, err := repos.Users.Create(ctx, &u)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	a := store.App{UserID: uid, Token: key30(2), Name: "app"}
	appID, err := repos.Apps.Create(ctx, &a)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}

	h := up.New(repos, stubResolver{userID: uid}, authSecret, up.Options{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)

	e := &env{
		t: t, h: h, srv: srv, repos: repos, st: st,
		userID: uid, appID: appID, hmacKey: authSecret,
	}
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		_ = st.Close()
	})
	return e
}

// key30 returns a deterministic 30-char numeric id valid for the CHECK
// constraints on user_key/token.
func key30(seed int) string { return strings.Repeat("0", 30-len(digits(seed))) + digits(seed) }

func digits(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// subscribeOK POSTs subscribe and asserts a 200 with valid device_id/secret/sub.
func (e *env) subscribeOK(name string) (deviceID, secret, sub string) {
	e.t.Helper()
	// Use an arbitrary valid placeholder sub in the path; the authoritative
	// sub is the derived one returned in the body.
	u := e.srv.URL + "/up/AAAA/subscribe.json"
	var body io.Reader
	if name != "" {
		body = strings.NewReader(url.Values{"name": {name}}.Encode())
	}
	req, _ := http.NewRequest(http.MethodPost, u, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("subscribe status=%d body=%s", resp.StatusCode, string(raw))
	}
	var res struct {
		Status   int    `json:"status"`
		DeviceID string `json:"device_id"`
		Secret   string `json:"secret"`
		Sub      string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		e.t.Fatalf("subscribe decode %q: %v", string(raw), err)
	}
	if res.Status != 1 || !idRe.MatchString(res.DeviceID) || !idRe.MatchString(res.Secret) {
		e.t.Fatalf("subscribe bad envelope: %+v", res)
	}
	if !subRe.MatchString(res.Sub) {
		e.t.Fatalf("subscribe sub = %q, want 4 chars [A-Za-z0-9]", res.Sub)
	}
	// The returned sub MUST equal DeriveSub(device_id) -- recomputed, not
	// hardcoded, so an HMAC output change is caught structurally.
	if want := up.DeriveSub(e.hmacKey, res.DeviceID); res.Sub != want {
		e.t.Fatalf("subscribe sub = %q, DeriveSub = %q", res.Sub, want)
	}
	return res.DeviceID, res.Secret, res.Sub
}

// seedMessages creates n fan-out rows for the env user (oldest first) and
// returns them with assigned IDs.
func (e *env) seedMessages(n int) []store.Message {
	e.t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		fanout := store.Fanout{
			Send: store.Send{
				AppID: e.appID, SenderUserID: e.userID, Priority: 0,
				Body: "body-" + digits(i), Timestamp: int64(i), CreatedAt: testTime,
			},
			Messages: []store.Message{{RecipientUserID: e.userID, CreatedAt: testTime}},
		}
		if _, err := e.repos.Sends.CreateFanout(ctx, &fanout); err != nil {
			e.t.Fatalf("seed message %d: %v", i, err)
		}
	}
	rows, err := e.repos.Messages.ListSince(ctx, e.userID, 0, n+5)
	if err != nil {
		e.t.Fatalf("list seeded: %v", err)
	}
	if len(rows) != n {
		e.t.Fatalf("expected %d seeded rows, got %d", n, len(rows))
	}
	return rows
}

// pullMessages GETs messages.json with the given since and decodes the array.
func (e *env) pullMessages(deviceID, secret, sub, since string) ([]up.Message, int) {
	e.t.Helper()
	u := e.srv.URL + "/up/" + sub + "/messages.json?device_id=" + deviceID +
		"&secret=" + secret + "&since=" + since
	resp, err := http.Get(u)
	if err != nil {
		e.t.Fatalf("pull: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var out []up.Message
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("pull decode %q: %v", string(raw), err)
	}
	return out, resp.StatusCode
}

// --- tests -----------------------------------------------------------------

// TestSubscribeCreatesDevice: subscribe returns a 30-char device_id, a 30-char
// secret, and a 4-char sub equal to DeriveSub(device_id).
func TestSubscribeCreatesDevice(t *testing.T) {
	e := newEnv(t)
	deviceID, secret, sub := e.subscribeOK("phone")

	if deviceID == secret {
		t.Fatalf("device_id == secret (both %q)", deviceID)
	}
	// Persisted: the device row exists with SHA-256(secret).
	dev, err := e.repos.Devices.GetByDeviceID(context.Background(), deviceID)
	if err != nil {
		t.Fatalf("device not persisted: %v", err)
	}
	if dev.UserID != e.userID {
		t.Fatalf("device user = %d, want %d", dev.UserID, e.userID)
	}
	if dev.Name != "phone" {
		t.Fatalf("device name = %q, want phone", dev.Name)
	}
	// Re-derive the sub from the returned device_id and compare (not hardcoded).
	if got := up.DeriveSub(e.hmacKey, deviceID); got != sub {
		t.Fatalf("DeriveSub = %q, response sub = %q", got, sub)
	}
}

// TestSubscribeRequiresSession: without a session resolver, subscribe is 401.
func TestSubscribeRequiresSession(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "up.db")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	repos := st.Repos()
	u := store.User{Email: "x@p.local", PassHash: "x", Role: "user", UserKey: key30(9), QuietTZ: "UTC", CreatedAt: testTime}
	if _, err := repos.Users.Create(ctx, &u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	h := up.New(repos, noResolver{}, authSecret, up.Options{})
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/up/AAAA/subscribe.json",
		"application/x-www-form-urlencoded", strings.NewReader("name=x"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// TestSubscribeBadName: a 26-char name is 400, a 25-char name is accepted.
func TestSubscribeBadName(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("a", 26)
	body := url.Values{"name": {long}}.Encode()
	resp, err := http.Post(e.srv.URL+"/up/AAAA/subscribe.json",
		"application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("26-char name status = %d, want 400", resp.StatusCode)
	}
	// 25-char name still succeeds (boundary).
	_, _, _ = e.subscribeOK(strings.Repeat("b", 25))
}

// TestSubscribeBadSubFormat: a path {sub} outside [A-Za-z0-9]{4} is 400.
func TestSubscribeBadSubFormat(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"abc", "ABCDE", "ab_d", "äbc"} {
		body := url.Values{"name": {"x"}}.Encode()
		resp, err := http.Post(e.srv.URL+"/up/"+bad+"/subscribe.json",
			"application/x-www-form-urlencoded", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %q: %v", bad, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("sub %q status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

// TestMessagesRequiresAuth: missing/wrong device credentials are 401.
func TestMessagesRequiresAuth(t *testing.T) {
	e := newEnv(t)
	deviceID, secret, sub := e.subscribeOK("phone")

	// No creds -> 401.
	if _, code := e.pullMessages("", "", sub, "0"); code != http.StatusUnauthorized {
		t.Fatalf("no creds status = %d, want 401", code)
	}
	// Wrong secret -> 401.
	if _, code := e.pullMessages(deviceID, "wrong"+secret, sub, "0"); code != http.StatusUnauthorized {
		t.Fatalf("wrong secret status = %d, want 401", code)
	}
}

// TestMessagesSubMismatch: authenticated device with a wrong path {sub} is 404
// (the subscription key does not match the device).
func TestMessagesSubMismatch(t *testing.T) {
	e := newEnv(t)
	deviceID, secret, _ := e.subscribeOK("phone")
	// Use a syntactically valid but wrong sub. It almost certainly differs
	// from the derived one; if it happens to collide, derive a guaranteed
	// different one by retrying is unnecessary -- assert != derived first.
	wrong := "ZZZZ"
	if wrong == up.DeriveSub(e.hmacKey, deviceID) {
		wrong = "YYYY" // astronomically unlikely double-collision
	}
	if _, code := e.pullMessages(deviceID, secret, wrong, "0"); code != http.StatusNotFound {
		t.Fatalf("wrong sub status = %d, want 404", code)
	}
}

// TestMessagesEmptyIsArray: an empty result is the JSON array "[]", not null.
func TestMessagesEmptyIsArray(t *testing.T) {
	e := newEnv(t)
	deviceID, secret, sub := e.subscribeOK("phone")
	out, code := e.pullMessages(deviceID, secret, sub, "0")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("empty pull = %v, want non-nil empty slice", out)
	}
}

// TestMessagesSinceReplay: pull with since=N returns only id>N, oldest first,
// capped at 100.
func TestMessagesSinceReplay(t *testing.T) {
	e := newEnv(t)
	msgs := e.seedMessages(3) // ids 1,2,3
	m1, m3 := msgs[0].ID, msgs[2].ID
	deviceID, secret, sub := e.subscribeOK("phone")

	// since=0 -> all three.
	page, code := e.pullMessages(deviceID, secret, sub, "0")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(page) != 3 {
		t.Fatalf("page len = %d, want 3", len(page))
	}
	for i := 1; i < len(page); i++ {
		if page[i].ID <= page[i-1].ID {
			t.Fatalf("page not ascending at %d", i)
		}
	}

	// since=m1 -> only m2,m3 (id > m1).
	page2, _ := e.pullMessages(deviceID, secret, sub, digits0(m1))
	if len(page2) != 2 {
		t.Fatalf("since=m1 len = %d, want 2", len(page2))
	}
	if page2[0].ID != msgs[1].ID || page2[1].ID != m3 {
		t.Fatalf("since=m1 ids = [%d,%d], want [%d,%d]",
			page2[0].ID, page2[1].ID, msgs[1].ID, m3)
	}
}

// digits0 stringifies an int64 id for the since query.
func digits0(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestAckIdempotent: ack always returns 200 {"status":1}, twice in a row.
func TestAckIdempotent(t *testing.T) {
	e := newEnv(t)
	deviceID, secret, sub := e.subscribeOK("phone")
	_ = deviceID
	_ = secret
	for i := 0; i < 2; i++ {
		u := e.srv.URL + "/up/" + sub + "/ack/42"
		resp, err := http.Post(u, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("ack %d: %v", i, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("ack %d status = %d, want 200", i, resp.StatusCode)
		}
		var env struct {
			Status int `json:"status"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Status != 1 {
			t.Fatalf("ack %d body %q: status != 1", i, string(raw))
		}
	}
}

// TestDeriveSubDeterministicAndFormatted: DeriveSub is stable, matches the
// format, and (probabilistically) differs across distinct device ids.
func TestDeriveSubDeterministicAndFormatted(t *testing.T) {
	id := "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123" // 32 -> trim to 30 below is not needed
	id = id[:30]
	a := up.DeriveSub(authSecret, id)
	b := up.DeriveSub(authSecret, id)
	if a != b {
		t.Fatalf("DeriveSub not deterministic: %q vs %q", a, b)
	}
	if !subRe.MatchString(a) {
		t.Fatalf("DeriveSub %q not 4-char [A-Za-z0-9]", a)
	}
	// A different input should (with overwhelming likelihood) yield a
	// different sub; this guards against a constant-output bug.
	other := up.DeriveSub(authSecret, strings.Repeat("z", 30))
	if other == a {
		t.Fatalf("DeriveSub collided for distinct inputs: %q", a)
	}
}
