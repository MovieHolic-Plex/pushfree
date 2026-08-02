package fcm

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// --- fakes ---

type fakeTokenSource struct {
	tok   string
	err   error
	calls int
}

func (f *fakeTokenSource) Token(context.Context) (string, error) {
	f.calls++
	return f.tok, f.err
}

type fakeDevices struct {
	mu      sync.Mutex
	cleared []string
	err     error
}

func (f *fakeDevices) ClearFCMToken(_ context.Context, deviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, deviceID)
	return f.err
}

func (f *fakeDevices) clearedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.cleared))
	copy(out, f.cleared)
	return out
}

type fakeRecorder struct {
	mu       sync.Mutex
	recorded []int64
	err      error
}

func (f *fakeRecorder) RecordDelivery(_ context.Context, msgID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, msgID)
	return f.err
}

func (f *fakeRecorder) ids() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.recorded))
	copy(out, f.recorded)
	return out
}

// requestCapture records the last FCM request the test server received.
type requestCapture struct {
	mu   sync.Mutex
	path string
	auth string
	body []byte
}

func (c *requestCapture) set(path, auth string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = path
	c.auth = auth
	c.body = body
}

func (c *requestCapture) snapshot() (string, string, []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := make([]byte, len(c.body))
	copy(b, c.body)
	return c.path, c.auth, b
}

// fcmServer returns an httptest.Server that captures the raw request and
// replies with status + respBody, so tests assert the exact FCM v1 wire
// format (priority mapping, data-only, bearer) without any real network.
func fcmServer(t *testing.T, status int, respBody string) (*httptest.Server, *requestCapture) {
	t.Helper()
	cap := &requestCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.set(r.URL.Path, r.Header.Get("Authorization"), b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cap
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tests ---

func TestAndroidPriorityMapping(t *testing.T) {
	cases := []struct {
		priority int
		want     string
	}{
		{2, "HIGH"},
		{1, "HIGH"},
		{0, "NORMAL"},
		{-1, "NORMAL"},
		{-2, "NORMAL"},
	}
	for _, tc := range cases {
		srv, cap := fcmServer(t, http.StatusOK, `{}`)
		rec := &fakeRecorder{}
		devs := &fakeDevices{}
		c := &Client{
			projectID: "proj-test",
			endpoint:  srv.URL,
			http:      srv.Client(),
			tokens:    &fakeTokenSource{tok: "fake-bearer"},
			devices:   devs,
			recorder:  rec,
			log:       quietLogger(),
		}
		res, err := c.Send(context.Background(), Outbound{
			MsgID: 7, DeviceID: "dev-A", Token: "tok-A", Priority: tc.priority,
			Data: map[string]string{"m": "hi"},
		})
		if err != nil {
			t.Fatalf("priority %d: Send error: %v", tc.priority, err)
		}
		if res.State != StateDelivered {
			t.Fatalf("priority %d: state = %s, want delivered", tc.priority, res.State)
		}

		path, auth, body := cap.snapshot()
		if want := "/v1/projects/proj-test/messages:send"; path != want {
			t.Errorf("priority %d: path = %q, want %q", tc.priority, path, want)
		}
		if auth != "Bearer fake-bearer" {
			t.Errorf("priority %d: Authorization = %q, want Bearer fake-bearer", tc.priority, auth)
		}
		// Raw body assertion: data-only + correct priority + token.
		if bytes.Contains(body, []byte(`"notification"`)) {
			t.Errorf("priority %d: request body contains a \"notification\" block; must be data-only:\n%s", tc.priority, body)
		}
		var req fcmRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("priority %d: unmarshal request body: %v\nraw: %s", tc.priority, err, body)
		}
		if req.Message.Token != "tok-A" {
			t.Errorf("priority %d: message.token = %q, want tok-A", tc.priority, req.Message.Token)
		}
		if req.Message.Android == nil {
			t.Fatalf("priority %d: android config missing\nraw: %s", tc.priority, body)
		}
		if req.Message.Android.Priority != tc.want {
			t.Errorf("priority %d: android.priority = %q, want %q\nraw: %s", tc.priority, req.Message.Android.Priority, tc.want, body)
		}
		if req.Message.Data["m"] != "hi" {
			t.Errorf("priority %d: data not forwarded: %+v", tc.priority, req.Message.Data)
		}
		// Delivery recorded exactly once on success.
		if ids := rec.ids(); len(ids) != 1 || ids[0] != 7 {
			t.Errorf("priority %d: delivery recorded = %v, want [7]", tc.priority, ids)
		}
		// Success must not invalidate the token.
		if got := devs.clearedIDs(); len(got) != 0 {
			t.Errorf("priority %d: token cleared on success: %v", tc.priority, got)
		}
	}
}

func TestTokenMintedEachSendIsCacheable(t *testing.T) {
	// The token source is called per send, but a real source caches. Here we
	// only assert the channel calls Token and forwards the result.
	srv, cap := fcmServer(t, http.StatusOK, `{}`)
	ts := &fakeTokenSource{tok: "abc"}
	c := &Client{
		projectID: "p", endpoint: srv.URL, http: srv.Client(),
		tokens: ts, log: quietLogger(),
	}
	if _, err := c.Send(context.Background(), Outbound{MsgID: 1, Token: "t", Priority: 0}); err != nil {
		t.Fatal(err)
	}
	_, auth, _ := cap.snapshot()
	if auth != "Bearer abc" {
		t.Errorf("auth = %q, want Bearer abc", auth)
	}
	if ts.calls != 1 {
		t.Errorf("token source calls = %d, want 1", ts.calls)
	}
}

func TestUnregisteredInvalidatesToken(t *testing.T) {
	resp := `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`
	srv, _ := fcmServer(t, http.StatusNotFound, resp)
	rec := &fakeRecorder{}
	devs := &fakeDevices{}
	c := &Client{
		projectID: "proj", endpoint: srv.URL, http: srv.Client(),
		tokens: &fakeTokenSource{tok: "t"}, devices: devs, recorder: rec,
		log: quietLogger(),
	}
	res, err := c.Send(context.Background(), Outbound{MsgID: 9, DeviceID: "dev-X", Token: "tok-X", Priority: 1})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if res.State != StateNotRegistered {
		t.Fatalf("state = %s, want not_registered", res.State)
	}
	if !strings.Contains(res.Reason, "UNREGISTERED") {
		t.Errorf("reason = %q, want it to contain UNREGISTERED", res.Reason)
	}
	if got := devs.clearedIDs(); len(got) != 1 || got[0] != "dev-X" {
		t.Errorf("ClearFCMToken calls = %v, want [dev-X]", got)
	}
	if ids := rec.ids(); len(ids) != 0 {
		t.Errorf("delivery recorded on failure: %v", ids)
	}
}

func TestInvalidArgumentInvalidatesToken(t *testing.T) {
	resp := `{"error":{"code":400,"message":"bad token","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"INVALID_ARGUMENT"}]}}`
	srv, _ := fcmServer(t, http.StatusBadRequest, resp)
	devs := &fakeDevices{}
	c := &Client{
		projectID: "proj", endpoint: srv.URL, http: srv.Client(),
		tokens: &fakeTokenSource{tok: "t"}, devices: devs, log: quietLogger(),
	}
	res, _ := c.Send(context.Background(), Outbound{MsgID: 1, DeviceID: "dev-I", Token: "tok-I", Priority: 0})
	if res.State != StateNotRegistered {
		t.Fatalf("state = %s, want not_registered", res.State)
	}
	if got := devs.clearedIDs(); len(got) != 1 || got[0] != "dev-I" {
		t.Errorf("ClearFCMToken calls = %v, want [dev-I]", got)
	}
}

func TestQuotaExceededBackoffAndClassifiedLog(t *testing.T) {
	resp := `{"error":{"code":429,"message":"Quota exceeded.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"QUOTA_EXCEEDED"}]}}`
	srv, _ := fcmServer(t, http.StatusTooManyRequests, resp)
	var logBuf bytes.Buffer
	devs := &fakeDevices{}
	c := &Client{
		projectID: "proj", endpoint: srv.URL, http: srv.Client(),
		tokens: &fakeTokenSource{tok: "t"}, devices: devs,
		log: slog.New(slog.NewJSONHandler(&logBuf, nil)),
	}
	res, err := c.Send(context.Background(), Outbound{MsgID: 1, DeviceID: "dev-Q", Token: "tok-Q", Priority: 2})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if res.State != StateBackoff {
		t.Fatalf("state = %s, want backoff", res.State)
	}
	if !strings.Contains(res.Reason, "QUOTA_EXCEEDED") {
		t.Errorf("reason = %q, want it to contain QUOTA_EXCEEDED", res.Reason)
	}
	// Backoff must NOT clear the token.
	if got := devs.clearedIDs(); len(got) != 0 {
		t.Errorf("QUOTA_EXCEEDED cleared token: %v", got)
	}
	// Classified log line: raw body classification is visible.
	logOut := logBuf.String()
	for _, want := range []string{"QUOTA_EXCEEDED", "backoff"} {
		if !strings.Contains(logOut, want) {
			t.Errorf("log missing %q:\n%s", want, logOut)
		}
	}
}

func TestTemporaryBannedIsBackoff(t *testing.T) {
	resp := `{"error":{"code":503,"message":"temporarily banned","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"TEMPORARY_BANNED"}]}}`
	srv, _ := fcmServer(t, http.StatusServiceUnavailable, resp)
	devs := &fakeDevices{}
	c := &Client{
		projectID: "proj", endpoint: srv.URL, http: srv.Client(),
		tokens: &fakeTokenSource{tok: "t"}, devices: devs, log: quietLogger(),
	}
	res, _ := c.Send(context.Background(), Outbound{MsgID: 1, DeviceID: "dev-B", Token: "tok-B", Priority: 1})
	if res.State != StateBackoff {
		t.Fatalf("state = %s, want backoff", res.State)
	}
	if !strings.Contains(res.Reason, "TEMPORARY_BANNED") {
		t.Errorf("reason = %q, want it to contain TEMPORARY_BANNED", res.Reason)
	}
	if got := devs.clearedIDs(); len(got) != 0 {
		t.Errorf("TEMPORARY_BANNED cleared token: %v", got)
	}
}

func TestHTTP5xxWithoutCodeIsBackoff(t *testing.T) {
	srv, _ := fcmServer(t, http.StatusInternalServerError, `{"error":{"message":"internal"}}`)
	c := &Client{
		projectID: "proj", endpoint: srv.URL, http: srv.Client(),
		tokens: &fakeTokenSource{tok: "t"}, log: quietLogger(),
	}
	res, _ := c.Send(context.Background(), Outbound{MsgID: 1, Token: "t", Priority: 0})
	if res.State != StateBackoff {
		t.Fatalf("state = %s, want backoff", res.State)
	}
}

func TestUnknown4xxIsFailed(t *testing.T) {
	resp := `{"error":{"code":403,"message":"forbidden","status":"PERMISSION_DENIED"}}`
	srv, _ := fcmServer(t, http.StatusForbidden, resp)
	devs := &fakeDevices{}
	c := &Client{
		projectID: "proj", endpoint: srv.URL, http: srv.Client(),
		tokens: &fakeTokenSource{tok: "t"}, devices: devs, log: quietLogger(),
	}
	res, _ := c.Send(context.Background(), Outbound{MsgID: 1, DeviceID: "dev-F", Token: "t", Priority: 0})
	if res.State != StateFailed {
		t.Fatalf("state = %s, want failed", res.State)
	}
	if got := devs.clearedIDs(); len(got) != 0 {
		t.Errorf("unknown 4xx cleared token: %v", got)
	}
}

func TestEmptyTokenIsFailed(t *testing.T) {
	srv, _ := fcmServer(t, http.StatusOK, `{}`)
	c := &Client{
		projectID: "proj", endpoint: srv.URL, http: srv.Client(),
		tokens: &fakeTokenSource{tok: "t"}, log: quietLogger(),
	}
	res, err := c.Send(context.Background(), Outbound{MsgID: 1, Token: "", Priority: 0})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if res.State != StateFailed {
		t.Fatalf("state = %s, want failed", res.State)
	}
}

// --- env-gating / construction ---

func TestMaybeNewDisabled(t *testing.T) {
	var buf bytes.Buffer
	c, err := MaybeNew(context.Background(), "", slog.New(slog.NewJSONHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("MaybeNew: %v", err)
	}
	if c != nil {
		t.Fatalf("want nil client when disabled, got %v", c)
	}
	if !strings.Contains(buf.String(), "disabled") {
		t.Errorf("missing disabled log line:\n%s", buf.String())
	}
}

func TestMaybeNewMissingFileErrors(t *testing.T) {
	_, err := MaybeNew(context.Background(), filepath.Join(t.TempDir(), "nope.json"), quietLogger())
	if err == nil {
		t.Fatal("want error for missing credentials file")
	}
}

func TestNewRejectsNonServiceAccount(t *testing.T) {
	f := writeTempJSON(t, map[string]any{
		"type": "user", "project_id": "p", "client_email": "x@y",
		"private_key": "-----BEGIN PRIVATE KEY-----\n-----END PRIVATE KEY-----\n",
	})
	_, err := New(context.Background(), Options{CredentialsPath: f})
	if err == nil {
		t.Fatal("want error for non-service_account credentials")
	}
}

func TestNewLoadsCredentials(t *testing.T) {
	key := generateTestKey(t)
	sa := map[string]any{
		"type":           "service_account",
		"project_id":     "proj-123",
		"private_key_id": "kid1",
		"private_key":    pemPKCS8(t, key),
		"client_email":   "svc@test.iam.gserviceaccount.com",
		"token_uri":      "https://oauth2.googleapis.com/token",
	}
	f := writeTempJSON(t, sa)
	c, err := New(context.Background(), Options{CredentialsPath: f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.projectID != "proj-123" {
		t.Errorf("projectID = %q, want proj-123", c.projectID)
	}
	if !c.Enabled() {
		t.Error("Enabled() = false, want true")
	}
	// The token source must be wired (not nil) so a real send can mint a token.
	if c.tokens == nil {
		t.Error("token source not wired")
	}
}

// TestNew_TokenExchange exercises the full stdlib JWT path against a fake
// OAuth2 token endpoint, then confirms a subsequent send reuses the cached
// token (one exchange for two sends). This validates the RS256 assertion and
// cache without real Google credentials.
func TestNew_TokenExchange(t *testing.T) {
	key := generateTestKey(t)
	sa := writeTempJSON(t, map[string]any{
		"type":         "service_account",
		"project_id":   "proj-live",
		"private_key":  pemPKCS8(t, key),
		"client_email": "svc@test.iam.gserviceaccount.com",
	})

	tokenCalls := 0
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("token content-type = %q", r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if gt := r.Form.Get("grant_type"); gt != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type = %q", gt)
		}
		assertion := r.Form.Get("assertion")
		if assertion == "" {
			t.Fatal("empty assertion")
		}
		parts := strings.Split(assertion, ".")
		if len(parts) != 3 {
			t.Fatalf("assertion has %d parts, want 3", len(parts))
		}
		// Verify the signature actually validates against the public key, so
		// the RS256 signing is correct end to end.
		signingInput := parts[0] + "." + parts[1]
		hashed := sha256.Sum256([]byte(signingInput))
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatalf("decode sig: %v", err)
		}
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, hashed[:], sig); err != nil {
			t.Fatalf("signature verification failed: %v", err)
		}
		_, _ = io.WriteString(w, `{"access_token":"live-token","expires_in":3600}`)
	}))
	t.Cleanup(tokenSrv.Close)

	c, err := New(context.Background(), Options{CredentialsPath: sa, HTTPClient: tokenSrv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Override the token + FCM endpoints to point at our test servers by
	// rebuilding the token source's URI would require plumbing; instead set
	// the tokenURI via a fresh load. Easier: point the whole client at a fake
	// FCM server and rely on the real token source hitting tokenSrv. The token
	// source keeps defaultTokenURI, so override it through a small rewrite:
	c.tokens.(*serviceAccountTokenSource).tokenURI = tokenSrv.URL

	fcmSrv, cap := fcmServer(t, http.StatusOK, `{}`)
	c.endpoint = fcmSrv.URL
	c.http = fcmSrv.Client()

	for i := 0; i < 2; i++ {
		res, err := c.Send(context.Background(), Outbound{MsgID: int64(i + 1), Token: "tok", Priority: 2})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if res.State != StateDelivered {
			t.Fatalf("send %d: state = %s", i, res.State)
		}
	}
	_, auth, _ := cap.snapshot()
	if auth != "Bearer live-token" {
		t.Errorf("auth = %q, want Bearer live-token", auth)
	}
	if tokenCalls != 1 {
		t.Errorf("token exchange calls = %d, want 1 (cached)", tokenCalls)
	}
}

// --- helpers ---

func writeTempJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return path
}

func generateTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// pemPKCS8 PEM-encodes an RSA private key as PKCS#8 (Google's format).
func pemPKCS8(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}
