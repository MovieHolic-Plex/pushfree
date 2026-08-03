package hub_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/pushfree/pushfree/internal/hub"
	"github.com/pushfree/pushfree/internal/metrics"
	"github.com/pushfree/pushfree/internal/store"
	"github.com/pushfree/pushfree/internal/store/sqlite"
)

// testTime is the fixed instant injected as the hub clock so the delivery-hook
// test can assert an exact delivered_at value.
var testTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// stubResolver is a SessionUserResolver that always resolves to one user. The
// real session middleware (todo 6) plugs in at wiring time (todo 8).
type stubResolver struct{ userID int64 }

func (s stubResolver) ResolveUserID(*http.Request) (int64, bool) { return s.userID, true }

// testEnv is a fully wired hub against a real temp-file SQLite store, served
// over httptest so transports are exercised exactly as in production.
type testEnv struct {
	t        *testing.T
	hub      *hub.Hub
	srv      *httptest.Server
	repos    store.Repos
	store    *sqlite.Store
	userID   int64
	appID    int64
	deviceID string
	secret   string
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvHook(t, nil)
}

// newEnvHook builds an env whose DeliveryHook is produced by hookBuilder
// (wrapping the default repo hook lets a test await hook completion). nil
// uses the default hook.
func newEnvHook(t *testing.T, hookBuilder func(store.Repos) hub.DeliveryHook) *testEnv {
	t.Helper()
	return newEnvWithOpts(t, hub.Options{
		// A long keepalive for general tests so keepalive frames do not flood
		// the fast assertions. TestKeepaliveInjected builds its own short-interval
		// env to exercise keepalive injection specifically.
		KeepaliveInterval: time.Hour,
		ReadTimeout:       5 * time.Second,
		Clock:             func() time.Time { return testTime },
	}, hookBuilder)
}

func newEnvWithOpts(t *testing.T, opts hub.Options, hookBuilder ...func(store.Repos) hub.DeliveryHook) *testEnv {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	repos := st.Repos()

	u := store.User{
		Email:     "u@pushfree.local",
		PassHash:  "x",
		Role:      "user",
		UserKey:   key30(1),
		QuietTZ:   "UTC",
		CreatedAt: testTime,
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

	// Apply the optional hook builder now that repos exists. hub.New otherwise
	// wires the default repo-backed hook.
	if len(hookBuilder) > 0 && hookBuilder[0] != nil {
		opts.DeliveryHook = hookBuilder[0](repos)
	}
	h := hub.New(repos, stubResolver{userID: uid}, opts)
	srv := httptest.NewServer(h.Routes())

	env := &testEnv{
		t: t, hub: h, srv: srv, repos: repos, store: st,
		userID: uid, appID: appID,
	}
	// Cleanup order matters: close the hub FIRST so every live transport loop
	// exits, then httptest.Server.Close() returns promptly (it otherwise blocks
	// on streaming handlers whose client-disconnect detection is unreliable on
	// some platforms), then close the DB.
	t.Cleanup(func() {
		h.Close()                    // signal live loops via done (handlers in select)
		srv.CloseClientConnections() // unblock any Write stuck on flow control
		srv.Close()                  // now returns promptly
		_ = st.Close()
	})

	env.deviceID, env.secret = env.registerDevice("phone")
	return env
}

// key30 returns a deterministic 30-char numeric id (valid for the 30-char
// CHECK constraints on user_key/token/receipt_id).
func key30(seed int) string {
	return fmt.Sprintf("%030d", seed)
}

func (e *testEnv) registerDevice(name string) (deviceID, secret string) {
	e.t.Helper()
	body := url.Values{"name": {name}, "os": {"test"}}.Encode()
	resp, err := http.Post(e.srv.URL+"/1/devices/login.json",
		"application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		e.t.Fatalf("register device: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("register device: status %d", resp.StatusCode)
	}
	var res struct {
		Status   int    `json:"status"`
		DeviceID string `json:"device_id"`
		Secret   string `json:"secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		e.t.Fatalf("decode register response: %v", err)
	}
	if res.Status != 1 || res.DeviceID == "" || res.Secret == "" {
		e.t.Fatalf("register response bad: %+v", res)
	}
	return res.DeviceID, res.Secret
}

// seedMessages creates n fan-out rows for the env user and returns them (with
// assigned IDs) oldest-first via ListSince.
func (e *testEnv) seedMessages(n int) []store.Message {
	e.t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		fanout := store.Fanout{
			Send: store.Send{
				AppID: e.appID, SenderUserID: e.userID, Priority: 0,
				Body: fmt.Sprintf("body-%d", i), Timestamp: int64(i), CreatedAt: testTime,
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

// seedMessageWithReceipt creates one p2 fan-out row carrying a receipt, and
// returns its message row (for delivered_at assertion) plus the receipt id.
func (e *testEnv) seedMessageWithReceipt(receiptID string) (store.Message, string) {
	e.t.Helper()
	ctx := context.Background()
	fanout := store.Fanout{
		Send: store.Send{
			AppID: e.appID, SenderUserID: e.userID, Priority: 2,
			Body: "emergency", Timestamp: 42, CreatedAt: testTime,
		},
		Messages: []store.Message{{RecipientUserID: e.userID, CreatedAt: testTime}},
		Receipt:  &store.Receipt{ID: receiptID, State: "pending"},
	}
	if _, err := e.repos.Sends.CreateFanout(ctx, &fanout); err != nil {
		e.t.Fatalf("seed receipt message: %v", err)
	}
	rows, err := e.repos.Messages.ListSince(ctx, e.userID, 0, 5)
	if err != nil {
		e.t.Fatalf("list after seed: %v", err)
	}
	return rows[len(rows)-1], receiptID
}

// dialWS opens a WS, sends the login line, and returns the conn + the parsed
// "open" frame.
func (e *testEnv) dialWS(t *testing.T, since, deviceID, secret string) (*websocket.Conn, wsFrame) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(e.srv.URL, "http://", "ws://", 1) +
		"/1/ws?since=" + since
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	login := fmt.Sprintf(`{"type":"login","device_id":%q,"secret":%q}`, deviceID, secret)
	if err := conn.Write(ctx, websocket.MessageText, []byte(login)); err != nil {
		t.Fatalf("ws write login: %v", err)
	}
	open := readWS(t, conn)
	if open.Type != "open" {
		t.Fatalf("expected open frame, got %+v", open)
	}
	return conn, open
}

// wsFrame is the subset of frame fields the tests inspect.
type wsFrame struct {
	Type          string `json:"type"`
	LastMessageID int64  `json:"last_message_id"`
	ID            int64  `json:"id"`
	Body          string `json:"message"`
	rawErr        error
}

// readWS reads one frame; on a transport error (e.g. close) rawErr is set.
func readWS(t *testing.T, conn *websocket.Conn) wsFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, line, err := conn.Read(ctx)
	if err != nil {
		return wsFrame{rawErr: err}
	}
	var f wsFrame
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatalf("decode ws frame %q: %v", string(line), err)
	}
	return f
}

// readWSMessage reads frames, skipping keepalives, until a message frame
// arrives (or an overall timeout). Keepalive frames may interleave with
// messages, so message assertions tolerate them rather than assume order.
func readWSMessage(t *testing.T, conn *websocket.Conn) wsFrame {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f := readWS(t, conn)
		if f.rawErr != nil {
			t.Fatalf("read message: %v", f.rawErr)
		}
		if f.Type == "message" {
			return f
		}
	}
	t.Fatal("no message frame within 3s")
	return wsFrame{}
}

// TestTwoClientFanout: publishing one message reaches two live WS clients of
// the same user.
func TestTwoClientFanout(t *testing.T) {
	e := newEnv(t)
	c1, _ := e.dialWS(t, "0", e.deviceID, e.secret)
	defer c1.Close(websocket.StatusNormalClosure, "")
	c2, _ := e.dialWS(t, "0", e.deviceID, e.secret)
	defer c2.Close(websocket.StatusNormalClosure, "")

	e.hub.Publish(e.userID, hub.StoredMessage{
		ID: 9991, SendID: 1, RecipientUserID: e.userID, Body: "hello-both",
	})

	for i, c := range []*websocket.Conn{c1, c2} {
		f := readWSMessage(t, c)
		if f.ID != 9991 || f.Body != "hello-both" {
			t.Fatalf("client %d got %+v, want message id=9991 hello-both", i, f)
		}
	}
}

// TestSinceReplayThenLiveJoin: a client connecting with since=N replays only
// id>N, then receives a live message published after the replay.
func TestSinceReplayThenLiveJoin(t *testing.T) {
	e := newEnv(t)
	msgs := e.seedMessages(3) // ids 1,2,3
	m1, m2, m3 := msgs[0].ID, msgs[1].ID, msgs[2].ID

	// since = m1: only m2 and m3 should be replayed (m1 is NOT > since).
	conn, open := e.dialWS(t, fmt.Sprintf("%d", m1), e.deviceID, e.secret)
	defer conn.Close(websocket.StatusNormalClosure, "")

	if open.LastMessageID != m3 {
		t.Fatalf("open last_message_id=%d, want %d", open.LastMessageID, m3)
	}

	// Replay: expect exactly m2 and m3, in order, and NOT m1.
	got := map[int64]bool{}
	for i := 0; i < 2; i++ {
		f := readWSMessage(t, conn)
		got[f.ID] = true
	}
	if got[m1] || !got[m2] || !got[m3] {
		t.Fatalf("replay set = %v, want only {%d,%d}", got, m2, m3)
	}

	// Live join: publish a message with id > everything replayed; it must
	// arrive exactly once (dedup must not drop it).
	e.hub.Publish(e.userID, hub.StoredMessage{
		ID: m3 + 100, SendID: 1, RecipientUserID: e.userID, Body: "live",
	})
	live := readWSMessage(t, conn)
	if live.ID != m3+100 || live.Body != "live" {
		t.Fatalf("live got %+v, want id=%d live", live, m3+100)
	}
}

// TestKeepaliveInjected: with a short injected keepalive interval, a connected
// client with no traffic receives a {"type":"keepalive"} frame.
func TestKeepaliveInjected(t *testing.T) {
	e := newEnvWithOpts(t, hub.Options{
		KeepaliveInterval: 25 * time.Millisecond, // short: keepalive must arrive quickly
		ReadTimeout:       5 * time.Second,
		Clock:             func() time.Time { return testTime },
	})
	conn, _ := e.dialWS(t, "0", e.deviceID, e.secret)
	defer conn.Close(websocket.StatusNormalClosure, "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f := readWS(t, conn)
		if f.rawErr != nil {
			t.Fatalf("read waiting for keepalive: %v", f.rawErr)
		}
		if f.Type == "keepalive" {
			return // success
		}
		// Any other frame type here is unexpected (no messages were seeded).
		t.Fatalf("unexpected frame waiting for keepalive: %+v", f)
	}
	t.Fatal("no keepalive frame received within 2s")
}

// TestWrongSecretRejected: a wrong secret closes WS with code 4001, and the
// HTTP pull/SSE endpoints return 401.
func TestWrongSecretRejected(t *testing.T) {
	e := newEnv(t)

	// WS: wrong secret -> close 4001.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(e.srv.URL, "http://", "ws://", 1) + "/1/ws"
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	login := fmt.Sprintf(`{"type":"login","device_id":%q,"secret":%q}`, e.deviceID, "wrong-secret")
	if err := conn.Write(ctx, websocket.MessageText, []byte(login)); err != nil {
		t.Fatalf("write login: %v", err)
	}
	_, _, rerr := conn.Read(ctx)
	code := websocket.CloseStatus(rerr)
	if code != 4001 {
		t.Fatalf("wrong-secret WS close code = %v (err=%v), want 4001", code, rerr)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// WS: unknown device -> close 4001.
	conn2, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	login2 := fmt.Sprintf(`{"type":"login","device_id":%q,"secret":%q}`, "no-such-device", e.secret)
	_ = conn2.Write(ctx, websocket.MessageText, []byte(login2))
	_, _, rerr2 := conn2.Read(ctx)
	if websocket.CloseStatus(rerr2) != 4001 {
		t.Fatalf("unknown-device WS close code = %v, want 4001", websocket.CloseStatus(rerr2))
	}
	_ = conn2.Close(websocket.StatusNormalClosure, "")

	// HTTP messages pull: wrong secret -> 401.
	res := httpGet(t, e.srv.URL+"/1/messages.json?device_id="+e.deviceID+"&secret=wrong")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("messages.json wrong secret status=%d, want 401", res.StatusCode)
	}
	res.Body.Close()

	// SSE: wrong secret -> 401.
	res2 := httpGet(t, e.srv.URL+"/1/sse?device_id="+e.deviceID+"&secret=wrong")
	if res2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sse wrong secret status=%d, want 401", res2.StatusCode)
	}
	res2.Body.Close()
}

func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// TestDeviceNameTooLong: a 26-char name is rejected with 400.
func TestDeviceNameTooLong(t *testing.T) {
	e := newEnv(t)
	longName := strings.Repeat("a", 26)
	body := url.Values{"name": {longName}}.Encode()
	resp, err := http.Post(e.srv.URL+"/1/devices/login.json",
		"application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("26-char name status=%d, want 400", resp.StatusCode)
	}
	var env struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Status != 0 || len(env.Errors) == 0 {
		t.Fatalf("error envelope = %+v, want status 0 with errors", env)
	}

	// A valid 25-char name still succeeds (boundary check).
	e.registerDevice(strings.Repeat("b", 25))
}

// TestMessagesPullSinceAndLimit: the HTTP pull respects the since cursor and
// the 100-row limit, returning messages oldest-first.
func TestMessagesPullSinceAndLimit(t *testing.T) {
	e := newEnv(t)
	msgs := e.seedMessages(105) // ids 1..105
	first := msgs[0].ID
	fifth := msgs[4].ID

	// since=0 -> first page of 100, oldest first (ids 1..100 relative).
	page := e.pullMessages("0")
	if len(page) != 100 {
		t.Fatalf("page length = %d, want 100", len(page))
	}
	if page[0].ID != first || page[99].ID != msgs[99].ID {
		t.Fatalf("page endpoints = [%d,%d], want [%d,%d]",
			page[0].ID, page[99].ID, first, msgs[99].ID)
	}
	for i := 1; i < len(page); i++ {
		if page[i].ID <= page[i-1].ID {
			t.Fatalf("page not ascending at %d: %d <= %d", i, page[i].ID, page[i-1].ID)
		}
	}

	// since = fifth -> only ids > fifth, capped at 100 (here 100 remain).
	page2 := e.pullMessages(fmt.Sprintf("%d", fifth))
	if len(page2) != 100 {
		t.Fatalf("since=fifth length = %d, want 100", len(page2))
	}
	if page2[0].ID != msgs[5].ID {
		t.Fatalf("since=fifth first = %d, want %d", page2[0].ID, msgs[5].ID)
	}
}

func (e *testEnv) pullMessages(since string) []hub.StoredMessage {
	e.t.Helper()
	u := fmt.Sprintf("%s/1/messages.json?device_id=%s&secret=%s&since=%s",
		e.srv.URL, e.deviceID, e.secret, since)
	resp := httpGet(e.t, u)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("pull status=%d", resp.StatusCode)
	}
	var out []hub.StoredMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		e.t.Fatalf("decode pull: %v", err)
	}
	return out
}

// signalingHook wraps a DeliveryHook and closes done the first time
// OnDelivered returns. It lets the delivery-hook test await the exact hook
// completion event (rather than racing the client frame read against the
// server-side DB writes) with a bounded timeout.
type signalingHook struct {
	hub.DeliveryHook
	done chan struct{}
	once sync.Once
}

func newSignalingHook(inner hub.DeliveryHook) *signalingHook {
	return &signalingHook{DeliveryHook: inner, done: make(chan struct{})}
}

func (s *signalingHook) OnDelivered(ctx context.Context, msg hub.StoredMessage) error {
	err := s.DeliveryHook.OnDelivered(ctx, msg)
	s.once.Do(func() { close(s.done) })
	return err
}

// TestDeliveryHookFiresOnWSAccept: when a WS replay delivers a message, the
// delivery hook sets messages.delivered_at (and, for a receipt send,
// receipts.last_delivered_at) to the injected clock value.
func TestDeliveryHookFiresOnWSAccept(t *testing.T) {
	var sig *signalingHook
	e := newEnvHook(t, func(repos store.Repos) hub.DeliveryHook {
		sig = newSignalingHook(hub.DefaultDeliveryHook(repos, func() time.Time { return testTime }))
		return sig
	})
	receiptID := key30(700)
	msg, _ := e.seedMessageWithReceipt(receiptID)

	// Connect with since=0 so the stored message is replayed over WS, which
	// is a transport accept and fires the hook.
	conn, open := e.dialWS(t, "0", e.deviceID, e.secret)
	defer conn.Close(websocket.StatusNormalClosure, "")
	if open.LastMessageID != msg.ID {
		t.Fatalf("open last_message_id=%d, want %d", open.LastMessageID, msg.ID)
	}
	f := readWSMessage(t, conn)
	if f.ID != msg.ID {
		t.Fatalf("replay frame = %+v, want message id=%d", f, msg.ID)
	}

	// Await the hook completion signal (bounded) before reading back rows.
	// The hook runs server-side after the frame is written; without this the
	// client frame read could race the DB writes.
	select {
	case <-sig.done:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery hook did not fire within 2s")
	}

	rows, err := e.repos.Messages.ListSince(context.Background(), e.userID, 0, 10)
	if err != nil {
		t.Fatalf("list after deliver: %v", err)
	}
	var delivered *time.Time
	for _, r := range rows {
		if r.ID == msg.ID {
			delivered = r.DeliveredAt
		}
	}
	if delivered == nil {
		t.Fatal("delivered_at not set after WS accept")
	}
	if !delivered.Equal(testTime) {
		t.Fatalf("delivered_at = %v, want injected %v", *delivered, testTime)
	}

	// The receipt's last_delivered_at should also be set.
	rcpt, err := e.repos.Receipts.GetByID(context.Background(), receiptID)
	if err != nil {
		t.Fatalf("get receipt: %v", err)
	}
	if rcpt.LastDeliveredAt == nil || !rcpt.LastDeliveredAt.Equal(testTime) {
		t.Fatalf("receipt last_delivered_at = %v, want %v", rcpt.LastDeliveredAt, testTime)
	}
}

// TestSSELiveDelivery is a smoke test that the SSE fallback streams a real
// message end-to-end (open event + live message event).
func TestSSELiveDelivery(t *testing.T) {
	e := newEnv(t)
	u := fmt.Sprintf("%s/1/sse?device_id=%s&secret=%s", e.srv.URL, e.deviceID, e.secret)
	// Use a cancelable context: cancelling it aborts the client connection,
	// which the server detects and uses to cancel its request context, letting
	// the streaming handler exit and httptest.Server.Close() (in t.Cleanup)
	// return promptly.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse GET: %v", err)
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status=%d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// First event must be "open".
	event, data, ok := readSSEEvent(scanner)
	if !ok || event != "open" {
		t.Fatalf("first sse event = %q/%q, want open", event, data)
	}

	// Publish a live message and expect an event: message line.
	e.hub.Publish(e.userID, hub.StoredMessage{
		ID: 4242, SendID: 1, RecipientUserID: e.userID, Body: "sse-live",
	})
	event, data, ok = readSSEEvent(scanner)
	if !ok || event != "message" {
		t.Fatalf("sse message event = %q/%q, want message", event, data)
	}
	var f wsFrame
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		t.Fatalf("decode sse data %q: %v", data, err)
	}
	if f.ID != 4242 || f.Body != "sse-live" {
		t.Fatalf("sse payload = %+v, want id=4242 sse-live", f)
	}
}

// readSSEEvent reads one SSE event (its event: name and data: payload) from a
// scanner over the response body. It skips comment/keepalive lines. Returns
// ok=false at EOF.
func readSSEEvent(scanner *bufio.Scanner) (event, data string, ok bool) {
	event, data = "", ""
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if event != "" || data != "" {
				return event, data, true
			}
			continue // blank line with no event yet
		}
		switch {
		case strings.HasPrefix(line, ":"):
			continue // comment / keepalive
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return "", "", false
	}
	return event, data, false
}

// TestServeWSUpgradesThroughRequestLogger is the F3 regression test for the
// live WS delivery defect. The production server wraps the hub's mux in
// metrics.RequestLogger; its statusWriter embedded http.ResponseWriter
// without exposing Hijacker or Unwrap(), so coder/websocket.Accept could not
// hijack and every /1/ws upgrade returned HTTP 501. The hub's own unit tests
// served h.Routes() directly (bypassing the middleware) and stayed green,
// masking the break. This test wraps h.Routes() in RequestLogger exactly as
// server.New does, then proves (a) the upgrade now succeeds and reaches the
// "open" frame, and (b) a message published via the ingest seam
// (PublishFanout) is delivered live to the connected client. Pre-fix it
// fails at the dial with "expected handshake response status code 101 but
// got 501".
func TestServeWSUpgradesThroughRequestLogger(t *testing.T) {
	e := newEnv(t)

	// Reproduce the live wiring: hub routes behind the request-logging
	// middleware whose statusWriter is the regression surface.
	bundle := metrics.NewBundle()
	wrapped := metrics.RequestLogger(slog.Default(), bundle.Metrics)(e.hub.Routes())
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)

	// (a) Upgrade + authorize via the login frame, through the middleware.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(srv.URL, "http://", "ws://", 1) + "/1/ws"
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("ws dial through RequestLogger failed (pre-fix: HTTP 501): %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "bye") }()
	login := fmt.Sprintf(`{"type":"login","device_id":%q,"secret":%q}`, e.deviceID, e.secret)
	if err := conn.Write(ctx, websocket.MessageText, []byte(login)); err != nil {
		t.Fatalf("write login: %v", err)
	}
	open := readWS(t, conn)
	if open.Type != "open" {
		t.Fatalf("expected open frame through middleware, got %+v", open)
	}

	// (b) Seed ONE message committed AFTER the dial (so it is outside the
	// replay page) and publish it through the ingest seam. It must arrive
	// live on the connected client with its real DB id.
	fanout := store.Fanout{
		Send: store.Send{
			AppID: e.appID, SenderUserID: e.userID, Priority: 2,
			Body: "F3 live regression push", Timestamp: 99, CreatedAt: testTime,
		},
		Messages: []store.Message{{RecipientUserID: e.userID, CreatedAt: testTime}},
	}
	sendID, err := e.repos.Sends.CreateFanout(ctx, &fanout)
	if err != nil {
		t.Fatalf("seed send: %v", err)
	}
	rows, err := e.repos.Messages.ListSince(ctx, e.userID, open.LastMessageID, 5)
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 new message row, got %d (%v)", len(rows), err)
	}
	seeded := rows[0]
	send := fanout.Send
	send.ID = sendID
	e.hub.PublishFanout(ctx, send, []store.Message{seeded})

	frame := readWSMessage(t, conn)
	if frame.Body != "F3 live regression push" || frame.ID != seeded.ID {
		t.Fatalf("live message not delivered through middleware: %+v want body=%q id=%d",
			frame, "F3 live regression push", seeded.ID)
	}
}
