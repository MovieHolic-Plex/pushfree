// Package callbacks_test exercises the todo-25 callback worker. This file is
// the RED phase of STRICT TDD: internal/callbacks does not exist yet, so it
// fails to build (see .omo/evidence/task-25-pushfree.txt). The implementation
// lands in the follow-up commit.
//
// What is asserted (the contract this package must satisfy):
//
//  1. SSRF allowlist. The default policy blocks loopback (IPv4 127/8 + IPv6
//     ::1), link-local 169.254.0.0/16 + fe80::/10, RFC1918 (10/8, 172.16/12,
//     192.168/16), IPv6 ULA fc00::/7, and the unspecified address. A callback
//     whose URL resolves to any blocked IP is rejected at enqueue time: no
//     callback row is created and no HTTP request is made. The
//     callback-allowed-hosts config overrides the blocklist for a named host.
//  2. Redirect re-validation. A 3xx to an internal target is blocked by the
//     HTTP client's CheckRedirect hook; the internal target is never contacted
//     and the attempt is treated as a failure.
//  3. 60s retry under an injected clock. A non-2xx response schedules the next
//     attempt exactly RetryInterval (60s) later; advancing the injected clock
//     and re-running ProcessDue drives the next attempt. Recovery (500 then
//  200. succeeds on the second attempt and records receipt.called_back_at.
//  4. Indefinite retry + DLQ observability. A permanently-failing URL is
//     retried at 60s intervals without bound; each failed attempt appends a dlq
//     row (observability, never an abort).
//  5. Per-host concurrency cap. At most HostConcurrency (default 4) requests
//     are in flight to one host:port at a time.
//
// All temporal behavior is driven by an injected clock and channel
// synchronization -- there are NO time.Sleep calls anywhere in this file.
package callbacks_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pushfree/pushfree/internal/api"
	"github.com/pushfree/pushfree/internal/callbacks"
	"github.com/pushfree/pushfree/internal/store"
)

// --- test doubles -----------------------------------------------------------

// fakeClock is a controllable clock. now() is the value the worker reads; add
// advances it. Guarded by a mutex so concurrent attempts read a stable instant.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeCB is the in-memory stand-in for a callbacks row.
type fakeCB struct {
	id          int64
	receiptID   string
	url         string
	state       string
	nextAttempt *time.Time
	attempts    int
}

type fakeDLQ struct {
	callbackID int64
	lastError  string
	at         time.Time
	attempts   int
}

// fakeStore is the in-memory callbacks.Store. It stands in for the SQLite
// CallbackWorkerRepo so the worker logic is exercised without a database.
// Guarded by a mutex because ProcessDue dispatches attempts concurrently.
type fakeStore struct {
	mu         sync.Mutex
	targets    map[string]callbacks.Target
	cbs        []fakeCB
	dlq        []fakeDLQ
	calledBack map[string]time.Time
	nextID     int64
}

func newFakeStore(target callbacks.Target) *fakeStore {
	return &fakeStore{
		targets:    map[string]callbacks.Target{target.Receipt.ID: target},
		calledBack: map[string]time.Time{},
	}
}

func (s *fakeStore) GetTarget(_ context.Context, receiptID string) (callbacks.Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.targets[receiptID]
	if !ok {
		return callbacks.Target{}, store.ErrNotFound
	}
	return t, nil
}

func (s *fakeStore) CreateCallback(_ context.Context, receiptID, u string, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	t := now
	s.cbs = append(s.cbs, fakeCB{
		id:          s.nextID,
		receiptID:   receiptID,
		url:         u,
		state:       "pending",
		nextAttempt: &t,
	})
	return s.nextID, nil
}

func (s *fakeStore) ListDue(_ context.Context, now time.Time, limit int) ([]store.Callback, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []store.Callback
	for _, c := range s.cbs {
		if c.state == "done" {
			continue
		}
		if c.nextAttempt != nil && c.nextAttempt.After(now) {
			continue
		}
		na := c.nextAttempt
		out = append(out, store.Callback{
			ID:            c.id,
			ReceiptID:     c.receiptID,
			URL:           c.url,
			State:         c.state,
			NextAttemptAt: na,
			Attempts:      c.attempts,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) MarkFailed(_ context.Context, id int64, next time.Time, attempts int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cbs {
		if s.cbs[i].id == id {
			s.cbs[i].state = "failed"
			s.cbs[i].nextAttempt = &next
			s.cbs[i].attempts = attempts
			return nil
		}
	}
	return store.ErrNotFound
}

func (s *fakeStore) MarkDone(_ context.Context, id int64, receiptID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cbs {
		if s.cbs[i].id == id {
			s.cbs[i].state = "done"
			s.cbs[i].nextAttempt = nil
			break
		}
	}
	s.calledBack[receiptID] = at
	return nil
}

func (s *fakeStore) RecordDLQ(_ context.Context, callbackID int64, lastErr string, at time.Time, attempts int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dlq = append(s.dlq, fakeDLQ{callbackID: callbackID, lastError: lastErr, at: at, attempts: attempts})
	return nil
}

// snapshot copies the mutable state for assertions.
func (s *fakeStore) snapshot() ([]fakeCB, []fakeDLQ, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cbs := make([]fakeCB, len(s.cbs))
	copy(cbs, s.cbs)
	dlq := make([]fakeDLQ, len(s.dlq))
	copy(dlq, s.dlq)
	return cbs, dlq, len(s.calledBack)
}

func (s *fakeStore) calledBackAt(receiptID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.calledBack[receiptID]
	return at, ok
}

// --- helpers ----------------------------------------------------------------

// receiptAt returns a pointer to t (storage convention: nil == "not yet").
func receiptAt(t time.Time) *time.Time { return &t }

// newWorkerWith builds a worker over fs with an injected clock, allowing the
// given hosts (loopback is blocked by default, so test servers must be named
// here), a real http.Client (so httptest servers work), and the injected
// resolver r (nil -> the worker's default net resolver, which returns IP
// literals without DNS).
func newWorkerWith(t *testing.T, fs *fakeStore, clk *fakeClock, allowed []string, r callbacks.Resolver) *callbacks.Worker {
	t.Helper()
	w := callbacks.NewWorker(fs, callbacks.Options{
		Clock:        clk.now,
		AllowedHosts: allowed,
		Resolver:     r,
	})
	if w == nil {
		t.Fatalf("NewWorker returned nil")
	}
	return w
}

// allowedHostsOf returns a slice containing the host[:port] of each server URL
// plus the bare hostnames, so a test server on loopback is permitted.
func allowedHostsOf(servers ...*httptest.Server) []string {
	var out []string
	for _, s := range servers {
		u, _ := url.Parse(s.URL)
		out = append(out, u.Host, u.Hostname())
	}
	return out
}

// --- SSRF pure tests --------------------------------------------------------

func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// IPv4 loopback (127/8) -- the whole range, not just 127.0.0.1.
		{"127.0.0.1", true},
		{"127.255.255.255", true},
		// Link-local 169.254/16 (cloud metadata, e.g. 169.254.169.254).
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		// RFC1918.
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		// 172.32/16 is NOT RFC1918 -> allowed.
		{"172.32.0.1", false},
		{"172.15.0.1", false},
		{"192.168.1.1", true},
		{"192.168.0.0", true},
		// Unspecified.
		{"0.0.0.0", true},
		// IPv6 loopback + ULA + link-local.
		{"::1", true},
		{"::ffff:127.0.0.1", true},
		{"fc00::1", true},
		{"fd12:3456:789a::1", true},
		{"fe80::1", true},
		// Public addresses are NOT blocked.
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		ip, err := netip.ParseAddr(c.ip)
		if err != nil {
			t.Fatalf("ParseAddr %q: %v", c.ip, err)
		}
		if got := callbacks.IsBlockedIP(ip); got != c.want {
			t.Errorf("IsBlockedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	ctx := context.Background()
	// A resolver that returns a public IP for "example.com" and a private IP
	// for "internal.example.com" (DNS-rebinding style: a public-looking
	// hostname that resolves to a blocked address).
	resolve := callbacks.Resolver(func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "example.com":
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		case "internal.example.com":
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		case "rebind.example.com":
			return []netip.Addr{
				netip.MustParseAddr("93.184.216.34"),
				netip.MustParseAddr("169.254.169.254"),
			}, nil
		}
		return nil, fmt.Errorf("unexpected host %q", host)
	})

	blocked := []string{
		"http://127.0.0.1/",
		"http://127.1.2.3:8080/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://192.168.1.1/admin",
		"http://[::1]/",
		"http://[fd00::1]/",
		"http://internal.example.com/", // resolves to 10.1.2.3
		"http://rebind.example.com/",   // resolves to a blocked IP (rebinding)
	}
	for _, u := range blocked {
		if err := callbacks.ValidateURL(ctx, u, nil, resolve); !errors.Is(err, callbacks.ErrSSRFBlocked) {
			t.Errorf("ValidateURL(%q) err = %v, want ErrSSRFBlocked", u, err)
		}
	}

	allowed := []string{
		"http://example.com/",          // resolves to public IP
		"https://example.com:443/hook", // https + port
	}
	for _, u := range allowed {
		if err := callbacks.ValidateURL(ctx, u, nil, resolve); err != nil {
			t.Errorf("ValidateURL(%q) err = %v, want nil", u, err)
		}
	}

	// allowed-hosts override: a loopback host is permitted when named.
	allow := map[string]bool{"127.0.0.1": true}
	if err := callbacks.ValidateURL(ctx, "http://127.0.0.1:1234/", allow, resolve); err != nil {
		t.Errorf("ValidateURL with allow-list err = %v, want nil (override)", err)
	}

	// Non-http(s) schemes are rejected outright.
	if err := callbacks.ValidateURL(ctx, "ftp://example.com/", nil, resolve); !errors.Is(err, callbacks.ErrSSRFBlocked) {
		t.Errorf("ValidateURL(ftp://) err = %v, want ErrSSRFBlocked", err)
	}
	if err := callbacks.ValidateURL(ctx, "file:///etc/passwd", nil, resolve); !errors.Is(err, callbacks.ErrSSRFBlocked) {
		t.Errorf("ValidateURL(file://) err = %v, want ErrSSRFBlocked", err)
	}
}

// --- worker hook-seam compile check ----------------------------------------

// TestWorkerSatisfiesAckHook is a compile-time guarantee that the worker plugs
// into the todo-23 ack hook seam (api.AckHook) without importing api in the
// callbacks package (structural satisfaction).
func TestWorkerSatisfiesAckHook(t *testing.T) {
	var _ api.AckHook = (*callbacks.Worker)(nil)
}

// --- happy path: 500 -> 200 after 60s (injected clock) ---------------------

func TestCallbackRetry500To200(t *testing.T) {
	receiptID := "R00000000000000000000000000001"
	var calls int32
	var gotBody []byte
	var bodyMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		gotBody = b
		bodyMu.Unlock()
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %d Content-Type = %q, want application/json", n, r.Header.Get("Content-Type"))
		}
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	target := callbacks.Target{
		Receipt: store.Receipt{ID: receiptID, State: "acknowledged",
			AcknowledgedAt: receiptAt(start)},
		CallbackURL: srv.URL,
	}
	fs := newFakeStore(target)
	w := newWorkerWith(t, fs, clk, allowedHostsOf(srv), nil)

	if _, err := w.Enqueue(context.Background(), receiptID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First attempt is driven by ProcessDue (NOT a blocking call in Enqueue):
	// the row is created with next_attempt_at = now, and ProcessDue claims it.
	res1, err := w.ProcessDue(context.Background())
	if err != nil {
		t.Fatalf("ProcessDue #1: %v", err)
	}
	if len(res1) != 1 || !failed(res1[0]) {
		t.Fatalf("ProcessDue #1 = %+v, want one failed result", res1)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("after first attempt, server calls = %d, want 1", got)
	}
	cbs, dlq, _ := fs.snapshot()
	if len(cbs) != 1 || cbs[0].state != "failed" || cbs[0].attempts != 1 {
		t.Fatalf("after failed attempt, callback = %+v, want state=failed attempts=1", cbs)
	}
	if len(dlq) != 1 {
		t.Fatalf("dlq rows = %d, want 1 (observability on failure)", len(dlq))
	}
	next := cbs[0].nextAttempt
	if next == nil {
		t.Fatalf("next_attempt_at is nil after failure")
	}
	if got, want := next.Sub(start), 60*time.Second; got != want {
		t.Fatalf("next_attempt_at offset = %s, want %s", got, want)
	}

	// Before the 60s elapse, ProcessDue is a no-op (not due yet).
	if res, _ := w.ProcessDue(context.Background()); len(res) != 0 {
		t.Fatalf("ProcessDue before 60s = %d results, want 0", len(res))
	}

	// Advance the injected clock by exactly 60s and drive the second attempt.
	clk.add(60 * time.Second)
	res2, err := w.ProcessDue(context.Background())
	if err != nil {
		t.Fatalf("ProcessDue #2: %v", err)
	}
	if len(res2) != 1 || !res2[0].Success {
		t.Fatalf("ProcessDue #2 = %+v, want one successful result", res2)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("after second attempt, server calls = %d, want 2", got)
	}
	// The POST body must carry the receipt id and a snapshot field.
	bodyMu.Lock()
	body := gotBody
	bodyMu.Unlock()
	if !bytes.Contains(body, []byte(receiptID)) {
		t.Errorf("POST body = %s, want it to contain receipt id %q", body, receiptID)
	}
	if !bytes.Contains(body, []byte("acknowledged")) {
		t.Errorf("POST body = %s, want an 'acknowledged' snapshot field", body)
	}

	// Success records receipt.called_back_at and marks the callback done.
	at, ok := fs.calledBackAt(receiptID)
	if !ok {
		t.Fatalf("called_back_at not set after 2xx")
	}
	if !at.Equal(clk.now()) {
		t.Errorf("called_back_at = %s, want %s (injected clock)", at, clk.now())
	}
	cbs, _, _ = fs.snapshot()
	if len(cbs) != 1 || cbs[0].state != "done" {
		t.Fatalf("callback state after success = %+v, want done", cbs)
	}

	// No further work: a done callback is not listed as due.
	if res, _ := w.ProcessDue(context.Background()); len(res) != 0 {
		t.Fatalf("ProcessDue after done = %d results, want 0", len(res))
	}
}

func failed(r callbacks.Result) bool { return !r.Success }

// --- SSRF: blocked at enqueue, never attempted -----------------------------

func TestCallbackSSRFBlocked(t *testing.T) {
	receiptID := "S00000000000000000000000000002"
	// A hit-counting transport: if SSRF validation leaks a real request, this
	// fails the test. The default client is wrapped so we can observe Do calls.
	var doCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&doCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clk := &fakeClock{t: time.Now()}
	target := callbacks.Target{
		Receipt:     store.Receipt{ID: receiptID, State: "acknowledged"},
		CallbackURL: "http://169.254.169.254/latest/meta-data/iam/",
	}
	fs := newFakeStore(target)
	// NOTE: the loopback test server is NOT in the allow-list, and neither is
	// 169.254.169.254, so the cloud-metadata URL must be rejected.
	w := newWorkerWith(t, fs, clk, nil, nil)

	_, err := w.Enqueue(context.Background(), receiptID)
	if !errors.Is(err, callbacks.ErrSSRFBlocked) {
		t.Fatalf("Enqueue(SSRF url) err = %v, want ErrSSRFBlocked", err)
	}
	cbs, _, _ := fs.snapshot()
	if len(cbs) != 0 {
		t.Fatalf("SSRF-blocked enqueue created %d callback rows, want 0", len(cbs))
	}
	// Drive due processing just to be sure nothing surfaces later.
	res, _ := w.ProcessDue(context.Background())
	if len(res) != 0 {
		t.Fatalf("ProcessDue after SSRF block = %d results, want 0", len(res))
	}
	if got := atomic.LoadInt32(&doCalls); got != 0 {
		t.Fatalf("server was contacted %d times, want 0 (SSRF must block before dial)", got)
	}
}

// --- redirect-to-internal re-validated -------------------------------------

func TestCallbackRedirectToInternalBlocked(t *testing.T) {
	receiptID := "D00000000000000000000000000003"
	// The redirect TARGET server. Its host:port is intentionally NOT in the
	// allow-list; if CheckRedirect fails to re-validate, this server gets hit
	// and the test fails.
	var targetHits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&targetHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	// The ORIGIN server: 302 -> the internal target. Only this origin is
	// allow-listed (by its host:port), so the redirect to a different port is
	// blocked (loopback, not allow-listed).
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/exfil", http.StatusFound)
	}))
	defer origin.Close()

	clk := &fakeClock{t: time.Now()}
	tg := callbacks.Target{
		Receipt:     store.Receipt{ID: receiptID, State: "acknowledged"},
		CallbackURL: origin.URL,
	}
	fs := newFakeStore(tg)
	// Allow ONLY the origin host:port; the target's different port -> blocked.
	originURL, _ := url.Parse(origin.URL)
	w := newWorkerWith(t, fs, clk, []string{originURL.Host}, nil)

	if _, err := w.Enqueue(context.Background(), receiptID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	res, err := w.ProcessDue(context.Background())
	if err != nil {
		t.Fatalf("ProcessDue: %v", err)
	}
	if len(res) != 1 || !failed(res[0]) {
		t.Fatalf("ProcessDue = %+v, want one failed result (redirect blocked)", res)
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("internal redirect target was contacted %d times, want 0", got)
	}
	// The failure schedules a 60s retry (redirect behavior may be transient).
	cbs, _, _ := fs.snapshot()
	if len(cbs) != 1 || cbs[0].state != "failed" || cbs[0].attempts != 1 {
		t.Fatalf("callback after blocked redirect = %+v, want failed/1 attempt", cbs)
	}
	if cbs[0].nextAttempt == nil {
		t.Fatalf("next_attempt_at nil; want a 60s retry scheduled")
	}
}

// --- permanent failure: indefinite 60s retry + DLQ -------------------------

func TestCallbackPermanentFailureRetriesAndDLQ(t *testing.T) {
	receiptID := "P00000000000000000000000000004"
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{t: start}
	target := callbacks.Target{
		Receipt:     store.Receipt{ID: receiptID, State: "acknowledged"},
		CallbackURL: srv.URL,
	}
	fs := newFakeStore(target)
	w := newWorkerWith(t, fs, clk, allowedHostsOf(srv), nil)

	if _, err := w.Enqueue(context.Background(), receiptID); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Drive 4 attempts, advancing the clock by exactly 60s between each. The
	// callback never succeeds, so retry must continue indefinitely.
	for attempt := 1; attempt <= 4; attempt++ {
		res, err := w.ProcessDue(context.Background())
		if err != nil {
			t.Fatalf("ProcessDue #%d: %v", attempt, err)
		}
		if len(res) != 1 || !failed(res[0]) {
			t.Fatalf("attempt #%d = %+v, want one failed result", attempt, res)
		}
		if got := atomic.LoadInt32(&calls); got != int32(attempt) {
			t.Fatalf("after %d attempts, server calls = %d", attempt, got)
		}
		_, dlq, _ := fs.snapshot()
		if len(dlq) != attempt {
			t.Fatalf("dlq rows after %d attempts = %d, want %d (one per failure)", attempt, len(dlq), attempt)
		}
		clk.add(60 * time.Second)
	}

	// After 4 failures the callback is still retrying (not aborted) at 60s.
	cbs, _, _ := fs.snapshot()
	if len(cbs) != 1 || cbs[0].state != "failed" || cbs[0].attempts != 4 {
		t.Fatalf("callback after 4 failures = %+v, want failed/4 attempts", cbs)
	}
}

// --- per-host concurrency cap (channel-synced, no sleeps) ------------------

func TestCallbackPerHostConcurrencyCap(t *testing.T) {
	const n = 8
	const cap = 4
	receiptID := "H00000000000000000000000000005"

	// Each handler: count in-flight (assert <= cap inline), record the max,
	// signal it has started, then block until the release channel closes.
	var inFlight int32
	var maxInFlight int32
	started := make(chan struct{}, n)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxInFlight)
			if cur <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, cur) {
				break
			}
		}
		if cur > int32(cap) {
			t.Errorf("in-flight %d exceeds cap %d", cur, cap)
		}
		started <- struct{}{}
		<-release
		atomic.AddInt32(&inFlight, -1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clk := &fakeClock{t: time.Now()}
	tg := callbacks.Target{
		Receipt:     store.Receipt{ID: receiptID, State: "acknowledged"},
		CallbackURL: srv.URL,
	}
	fs := newFakeStore(tg)
	// Seed n callbacks to the SAME host:port by creating rows directly through
	// the store, so ProcessDue sees n due items to one host.
	for i := 0; i < n; i++ {
		if _, err := fs.CreateCallback(context.Background(), receiptID, srv.URL, clk.now()); err != nil {
			t.Fatalf("CreateCallback #%d: %v", i, err)
		}
	}

	w := callbacks.NewWorker(fs, callbacks.Options{
		Clock:           clk.now,
		AllowedHosts:    allowedHostsOf(srv),
		HostConcurrency: cap,
		DueLimit:        n,
		RetryInterval:   60 * time.Second,
	})
	if w == nil {
		t.Fatalf("NewWorker returned nil")
	}

	// Run ProcessDue in the background. The semaphore lets exactly `cap`
	// requests start; they all block on `release`. Wait until `cap` have
	// started -- this proves `cap` are running concurrently.
	done := make(chan struct{})
	go func() {
		_, _ = w.ProcessDue(context.Background())
		close(done)
	}()
	for i := 0; i < cap; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/%d handlers started before timeout (cap not reached)", i, cap)
		}
	}
	// Release all; the remaining n-cap proceed (still capped at `cap`).
	close(release)
	<-done

	if got := atomic.LoadInt32(&maxInFlight); got != int32(cap) {
		t.Errorf("max concurrent in-flight = %d, want %d", got, cap)
	}
}

// --- no callback_url configured -> no-op -----------------------------------

func TestCallbackNoURL(t *testing.T) {
	receiptID := "N00000000000000000000000000006"
	clk := &fakeClock{t: time.Now()}
	target := callbacks.Target{
		Receipt:     store.Receipt{ID: receiptID, State: "acknowledged"},
		CallbackURL: "", // send had no callback_url
	}
	fs := newFakeStore(target)
	w := newWorkerWith(t, fs, clk, nil, nil)

	id, err := w.Enqueue(context.Background(), receiptID)
	if err != nil {
		t.Fatalf("Enqueue with no URL: %v", err)
	}
	if id != 0 {
		t.Errorf("Enqueue id = %d, want 0 (no row created)", id)
	}
	cbs, _, _ := fs.snapshot()
	if len(cbs) != 0 {
		t.Errorf("created %d callback rows, want 0", len(cbs))
	}
}
