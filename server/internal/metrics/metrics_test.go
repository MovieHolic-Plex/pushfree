package metrics

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// wantedNames is the exact set of pushfree_* metric families /metrics MUST
// expose (SPEC todo 15). go_* and process_* are also present but are not
// asserted here (they vary by platform/runtime).
func wantedNames() []string {
	return []string{
		NameSends,
		NameMessagesReceived,
		NameDeliveryAttempts,
		NameDeliveryFailures,
		NameWSClients,
		NameAck,
	}
}

// TestBundleRegistersAllCollectors asserts every SPEC metric family is
// present in the gathered output, so a typo'd or dropped name is caught.
// Per Prometheus semantics, a CounterVec exposes no series until a label
// combination is instantiated, so one value per vec is touched before
// gathering.
func TestBundleRegistersAllCollectors(t *testing.T) {
	b := NewBundle()
	b.IncDeliveryAttempts("ws")
	b.IncDeliveryFailures("ws", "timeout")
	b.IncAck("ok")
	got, err := b.Registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	have := make(map[string]bool, len(got))
	for _, mf := range got {
		have[mf.GetName()] = true
	}
	for _, name := range wantedNames() {
		if !have[name] {
			t.Errorf("metric %q not registered; have %v", name, have)
		}
	}
}

// TestIncrementMethods checks each typed method moves the right counter, in
// isolation, on a fresh registry.
func TestIncrementMethods(t *testing.T) {
	b := NewBundle()

	b.IncSends()
	if c := testutil.ToFloat64(b.sends); c != 1 {
		t.Errorf("sends after IncSends = %v, want 1", c)
	}
	b.IncMessagesReceived()
	if c := testutil.ToFloat64(b.messagesReceived); c != 1 {
		t.Errorf("messages_received after IncMessagesReceived = %v, want 1", c)
	}
	b.IncDeliveryAttempts("ws")
	b.IncDeliveryAttempts("sse")
	if c := testutil.ToFloat64(b.deliveryAttempts.WithLabelValues("ws")); c != 1 {
		t.Errorf("delivery_attempts{ws} = %v, want 1", c)
	}
	b.IncDeliveryFailures("fcm", "unregistered")
	if c := testutil.ToFloat64(b.deliveryFailures.WithLabelValues("fcm", "unregistered")); c != 1 {
		t.Errorf("delivery_failures{fcm,unregistered} = %v, want 1", c)
	}
	b.SetWSClients(3)
	if c := testutil.ToFloat64(b.wsClients); c != 3 {
		t.Errorf("ws_clients after Set(3) = %v, want 3", c)
	}
	b.IncWSClients()
	if c := testutil.ToFloat64(b.wsClients); c != 4 {
		t.Errorf("ws_clients after Inc = %v, want 4", c)
	}
	b.DecWSClients()
	if c := testutil.ToFloat64(b.wsClients); c != 3 {
		t.Errorf("ws_clients after Dec = %v, want 3", c)
	}
	b.IncAck("ok")
	if c := testutil.ToFloat64(b.ack.WithLabelValues("ok")); c != 1 {
		t.Errorf("ack{ok} = %v, want 1", c)
	}
}

// TestMetricsHandlerServesText runs the real /metrics handler end-to-end and
// confirms the pushfree_* families appear in the Prometheus text format.
func TestMetricsHandlerServesText(t *testing.T) {
	b := NewBundle()
	b.IncMessagesReceived()
	b.IncDeliveryAttempts("ws")
	b.IncDeliveryFailures("ws", "timeout")
	b.IncAck("ok")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	b.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range wantedNames() {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics body missing %q", name)
		}
	}
	// The increment above must be reflected in the exposed text.
	if !strings.Contains(body, "pushfree_messages_received_total 1") {
		t.Errorf("/metrics body does not reflect messages_received=1:\n%s", body)
	}
}

// newCapturingLogger builds a slog JSON logger writing to a buffer so a test
// can assert the exact fields the middleware emits.
func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// TestRequestLoggerRequestIDCorrelation proves the SPEC correlation
// requirement: the SAME request_id appears in the X-Request-ID response
// header and in the JSON request log line.
func TestRequestLoggerRequestIDCorrelation(t *testing.T) {
	b := NewBundle()
	logger, buf := newCapturingLogger()
	h := RequestLogger(logger, b.Metrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	h.ServeHTTP(rec, req)

	headerID := rec.Header().Get(RequestIDHeader)
	if headerID == "" {
		t.Fatal("X-Request-ID response header is empty")
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not JSON: %v (raw=%q)", err, buf.String())
	}
	logID, _ := entry["request_id"].(string)
	if logID != headerID {
		t.Errorf("request_id mismatch: header=%q log=%q", headerID, logID)
	}
	if m, _ := entry["method"].(string); m != "GET" {
		t.Errorf("log method = %q, want GET", m)
	}
	if p, _ := entry["path"].(string); p != "/health" {
		t.Errorf("log path = %q, want /health", p)
	}
	if s, _ := entry["status"].(float64); s != 200 {
		t.Errorf("log status = %v, want 200", s)
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Errorf("log line missing duration_ms: %s", buf.String())
	}
}

// TestRequestLoggerHonorsInboundID confirms an inbound X-Request-ID is reused
// (not replaced), so an upstream proxy/caller can correlate across hops.
func TestRequestLoggerHonorsInboundID(t *testing.T) {
	b := NewBundle()
	logger, buf := newCapturingLogger()
	h := RequestLogger(logger, b.Metrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const inbound = "upstream-trace-123"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set(RequestIDHeader, inbound)
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get(RequestIDHeader); got != inbound {
		t.Errorf("response id = %q, want reused %q", got, inbound)
	}
	var entry map[string]any
	_ = json.Unmarshal(buf.Bytes(), &entry)
	if logID, _ := entry["request_id"].(string); logID != inbound {
		t.Errorf("log id = %q, want reused %q", logID, inbound)
	}
}

// TestRequestLoggerCountsAcceptedSend verifies the happy-path acceptance
// scenario: a 2xx POST /1/messages.json increments both sends and
// messages_received by exactly one.
func TestRequestLoggerCountsAcceptedSend(t *testing.T) {
	b := NewBundle()
	logger, _ := newCapturingLogger()
	h := RequestLogger(logger, b.Metrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	beforeSends := testutil.ToFloat64(b.sends)
	beforeRecv := testutil.ToFloat64(b.messagesReceived)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/1/messages.json", strings.NewReader("token=x&user=y&message=hi"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if c := testutil.ToFloat64(b.sends); c != beforeSends+1 {
		t.Errorf("sends delta = %v, want 1", c-beforeSends)
	}
	if c := testutil.ToFloat64(b.messagesReceived); c != beforeRecv+1 {
		t.Errorf("messages_received delta = %v, want 1", c-beforeRecv)
	}
}

// TestRequestLoggerSkipsNon2xxSend verifies the failure/edge scenario: a 400
// (validation) or 401 (auth) send does NOT move the accepted-send counters.
func TestRequestLoggerSkipsNon2xxSend(t *testing.T) {
	b := NewBundle()
	logger, _ := newCapturingLogger()
	h := RequestLogger(logger, b.Metrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))

	beforeSends := testutil.ToFloat64(b.sends)
	beforeRecv := testutil.ToFloat64(b.messagesReceived)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/1/messages.json", strings.NewReader("token="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, req)

	if c := testutil.ToFloat64(b.sends); c != beforeSends {
		t.Errorf("sends moved on 400: delta = %v, want 0", c-beforeSends)
	}
	if c := testutil.ToFloat64(b.messagesReceived); c != beforeRecv {
		t.Errorf("messages_received moved on 400: delta = %v, want 0", c-beforeRecv)
	}
}

// TestRequestLoggerDefaultStatusOnWrite confirms a handler that writes a body
// without WriteHeader is logged as 200 (matching net/http), so such handlers
// still count as accepted sends.
func TestRequestLoggerDefaultStatusOnWrite(t *testing.T) {
	b := NewBundle()
	logger, _ := newCapturingLogger()
	h := RequestLogger(logger, b.Metrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok") // no WriteHeader
	}))

	before := testutil.ToFloat64(b.messagesReceived)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/1/messages.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if c := testutil.ToFloat64(b.messagesReceived); c != before+1 {
		t.Errorf("messages_received delta = %v, want 1 (implicit 200)", c-before)
	}
}

// TestRequestLoggerWSClientsGauge confirms a GET /1/ws connection moves the
// gauge to 1 while the handler runs and back to 0 after it returns.
func TestRequestLoggerWSClientsGauge(t *testing.T) {
	b := NewBundle()
	logger, _ := newCapturingLogger()

	inFlight := make(chan struct{})
	released := make(chan struct{})
	h := RequestLogger(logger, b.Metrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(inFlight)
		<-released // hold the "connection" open until the test reads the gauge
	}))

	if c := testutil.ToFloat64(b.wsClients); c != 0 {
		t.Fatalf("ws_clients before connect = %v, want 0", c)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/1/ws", nil)
		h.ServeHTTP(rec, req)
	}()

	<-inFlight
	if c := testutil.ToFloat64(b.wsClients); c != 1 {
		t.Errorf("ws_clients while connected = %v, want 1", c)
	}
	close(released)
	<-done
	if c := testutil.ToFloat64(b.wsClients); c != 0 {
		t.Errorf("ws_clients after disconnect = %v, want 0", c)
	}
}

// TestRequestLoggerNilLoggerDoesNotPanic confirms a nil logger falls back to
// the default rather than nil-dereferencing.
func TestRequestLoggerNilLoggerDoesNotPanic(t *testing.T) {
	b := NewBundle()
	h := RequestLogger(nil, b.Metrics)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	h.ServeHTTP(rec, req)
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Error("nil logger path did not set X-Request-ID")
	}
}
