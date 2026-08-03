// This file is the GREEN phase of todo 25: the callback worker that makes
// worker_test.go pass. It owns the retry/SSRF orchestration and the narrow
// persistence surface (Store); the pure SSRF predicate lives in ssrf.go and
// the concrete SQLite implementation in internal/store/sqlite/callback_worker.go.
//
// Execution model. The worker is NOT a background loop by itself: it exposes
// Enqueue (called from the ack hook seam) and ProcessDue (the testable unit
// that claims and attempts every due callback). Run is a thin ticker wrapper
// around ProcessDue for production wiring; tests drive ProcessDue directly
// under an injected clock so the 60s retry cadence is exercised with zero real
// sleeps. The retry schedule is persisted in the callbacks table
// (next_attempt_at), so a crash between attempts loses no work: the next
// ProcessDue reclaims due rows. (The kill -9 chaos suite is todo 26.)
//
// Concurrency. ProcessDue dispatches each due callback in its own goroutine so
// independent hosts proceed in parallel, but a per-host:port semaphore
// (HostConcurrency, default 4) caps concurrent in-flight requests to any one
// endpoint, matching the plan's per-host backpressure requirement.
package callbacks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// DefaultRetryInterval is the non-2xx retry cadence (the plan's "1 min
// non-2xx"). It is the floor for the next attempt after a failure.
const DefaultRetryInterval = 60 * time.Second

// DefaultHostConcurrency caps concurrent in-flight callbacks to one
// host:port. Per the plan's SSRF+backpressure requirement.
const DefaultHostConcurrency = 4

// Target is the resolved callback target for one receipt: the receipt row
// (source of the POST body) and the parent send's callback_url (destination).
// GetTarget joins receipts to sends to populate both in one read.
type Target struct {
	Receipt     store.Receipt
	CallbackURL string
}

// Store is the narrow persistence surface the worker needs, defined here so
// the callbacks package (and its tests) stay decoupled from the concrete
// SQLite driver -- mirroring the RetryStore / CancelStore seams in the
// receipts package. The production implementation is
// internal/store/sqlite.CallbackWorkerRepo; the in-memory fake in
// worker_test.go stands in for a real database.
type Store interface {
	// GetTarget loads the receipt and its parent send's callback_url.
	GetTarget(ctx context.Context, receiptID string) (Target, error)
	// CreateCallback inserts a pending callback row due at now (attempts=0)
	// and returns its id.
	CreateCallback(ctx context.Context, receiptID, url string, now time.Time) (int64, error)
	// ListDue returns up to limit due callbacks: state != done and
	// (next_attempt_at IS NULL OR next_attempt_at <= now). Ordering is
	// implementation-defined but stable.
	ListDue(ctx context.Context, now time.Time, limit int) ([]store.Callback, error)
	// MarkFailed records a failed attempt: state=failed, attempts=attempts,
	// next_attempt_at=next.
	MarkFailed(ctx context.Context, id int64, next time.Time, attempts int) error
	// MarkDone marks the callback done (state=done, next_attempt_at=NULL) and
	// sets receipt.called_back_at=at in the same operation.
	MarkDone(ctx context.Context, id int64, receiptID string, at time.Time) error
	// RecordDLQ appends a dead-letter row for observability. It never aborts
	// retry: the callback remains due at its scheduled next_attempt_at.
	RecordDLQ(ctx context.Context, callbackID int64, lastErr string, at time.Time, attempts int) error
}

// Options configures a Worker. The zero value is NOT valid; use NewWorker,
// which applies defaults. Every temporal input is injectable so tests run
// without real sleeps or DNS.
type Options struct {
	// RetryInterval is the gap between failed attempts. <=0 -> 60s.
	RetryInterval time.Duration
	// Clock returns the current instant. nil -> time.Now. Tests inject a
	// controllable clock.
	Clock func() time.Time
	// Resolver maps a hostname to IPs. nil -> the system resolver.
	Resolver Resolver
	// AllowedHosts are hosts (bare hostname and/or host:port) that bypass the
	// SSRF denylist. nil/empty -> denylist fully enforced (loopback etc.
	// blocked).
	AllowedHosts []string
	// HostConcurrency caps concurrent in-flight requests to one host:port.
	// <=0 -> 4.
	HostConcurrency int
	// DueLimit caps how many due callbacks one ProcessDue claim reads. <=0 ->
	// 100.
	DueLimit int
	// PollInterval is the Run-loop tick. <=0 -> 5s. Not used by tests.
	PollInterval time.Duration
	// RequestTimeout caps one HTTP POST. <=0 -> 15s.
	RequestTimeout time.Duration
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Result records one attempted callback. Status is the HTTP status (0 on a
// transport/SSRF error); Success is true only for a 2xx response whose
// MarkDone persisted.
type Result struct {
	CallbackID int64
	Status     int
	Success    bool
	Err        error
}

// Worker delivers acknowledged-receipt callbacks with SSRF protection and 60s
// retry. It satisfies api.AckHook structurally via OnAcknowledged.
type Worker struct {
	store           Store
	retry           time.Duration
	clock           func() time.Time
	resolver        Resolver
	allowed         map[string]bool
	hostConcurrency int
	dueLimit        int
	pollInterval    time.Duration
	logger          *slog.Logger
	httpClient      *http.Client
}

// NewWorker builds a Worker over s with opts normalized. It constructs an
// HTTP client whose CheckRedirect re-validates each redirect target through
// ValidateURL, so a redirect to an internal address is blocked before dial.
func NewWorker(s Store, opts Options) *Worker {
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = DefaultRetryInterval
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Resolver == nil {
		opts.Resolver = defaultResolver
	}
	if opts.HostConcurrency <= 0 {
		opts.HostConcurrency = DefaultHostConcurrency
	}
	if opts.DueLimit <= 0 {
		opts.DueLimit = 100
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 5 * time.Second
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 15 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	allowed := make(map[string]bool, len(opts.AllowedHosts))
	for _, h := range opts.AllowedHosts {
		if h != "" {
			allowed[h] = true
		}
	}
	w := &Worker{
		store:           s,
		retry:           opts.RetryInterval,
		clock:           opts.Clock,
		resolver:        opts.Resolver,
		allowed:         allowed,
		hostConcurrency: opts.HostConcurrency,
		dueLimit:        opts.DueLimit,
		pollInterval:    opts.PollInterval,
		logger:          opts.Logger,
	}
	w.httpClient = &http.Client{
		Timeout: opts.RequestTimeout,
		// CheckRedirect re-validates each redirect Location through the SSRF
		// policy. A blocked target aborts the redirect chain before the client
		// dials it, so the internal address is never contacted.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := w.validate(req.Context(), req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
	return w
}

// validate is the in-package SSRF gate (initial URL + redirect targets).
func (w *Worker) validate(ctx context.Context, rawURL string) error {
	return ValidateURL(ctx, rawURL, w.allowed, w.resolver)
}

// OnAcknowledged is the todo-23 ack-hook seam. It enqueues a callback for the
// receipt (if its send has a callback_url) and is otherwise a no-op. It
// returns the enqueue error (e.g. ErrSSRFBlocked) for the ack path to log;
// the receipt is already acknowledged, so a non-nil error NEVER fails the ack
// HTTP response (the api package logs it best-effort).
//
// OnAcknowledged intentionally does NOT run the first attempt inline: the ack
// request should not block on a webhook POST. The Run loop (or a test calling
// ProcessDue) performs the delivery promptly.
func (w *Worker) OnAcknowledged(ctx context.Context, receiptID string) error {
	_, err := w.Enqueue(ctx, receiptID)
	return err
}

// Enqueue creates a pending callback row for receiptID iff its send has a
// callback_url that passes the SSRF gate. It returns (0, nil) when there is no
// callback_url (nothing to do), (0, ErrSSRFBlocked) when the URL is denied --
// in which case no row is created -- and (id, nil) on a successful insert.
func (w *Worker) Enqueue(ctx context.Context, receiptID string) (int64, error) {
	tgt, err := w.store.GetTarget(ctx, receiptID)
	if err != nil {
		return 0, fmt.Errorf("callback enqueue: load target %s: %w", receiptID, err)
	}
	if tgt.CallbackURL == "" {
		// The sender did not request a callback; nothing to do.
		return 0, nil
	}
	if err := w.validate(ctx, tgt.CallbackURL); err != nil {
		return 0, err
	}
	id, err := w.store.CreateCallback(ctx, receiptID, tgt.CallbackURL, w.clock())
	if err != nil {
		return 0, fmt.Errorf("callback enqueue: create row %s: %w", receiptID, err)
	}
	return id, nil
}

// ProcessDue claims and attempts every due callback at the injected clock's
// current instant. Each callback is attempted in its own goroutine so distinct
// hosts proceed in parallel, but a per-host:port semaphore caps concurrency.
// It returns one Result per attempted callback (in due-order) and blocks
// until all attempts finish.
func (w *Worker) ProcessDue(ctx context.Context) ([]Result, error) {
	now := w.clock()
	due, err := w.store.ListDue(ctx, now, w.dueLimit)
	if err != nil {
		return nil, fmt.Errorf("callback process: list due: %w", err)
	}
	if len(due) == 0 {
		return nil, nil
	}
	results := make([]Result, len(due))
	// Per-host:port semaphores, allocated lazily. A buffered channel of size
	// HostConcurrency gates concurrent in-flight requests to one endpoint.
	var semMu sync.Mutex
	sems := make(map[string]chan struct{}, 8)
	semFor := func(host string) chan struct{} {
		semMu.Lock()
		defer semMu.Unlock()
		c, ok := sems[host]
		if !ok {
			c = make(chan struct{}, w.hostConcurrency)
			sems[host] = c
		}
		return c
	}
	var wg sync.WaitGroup
	for i, cb := range due {
		wg.Add(1)
		go func(i int, cb store.Callback) {
			defer wg.Done()
			sem := semFor(hostKey(cb.URL))
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = Result{CallbackID: cb.ID, Err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			results[i] = w.attempt(ctx, cb)
		}(i, cb)
	}
	wg.Wait()
	return results, nil
}

// attempt performs one callback POST and records the outcome. A 2xx response
// marks the callback done and records receipt.called_back_at. Any other
// outcome (non-2xx, transport error, SSRF block on a redirect) schedules a
// retry one RetryInterval later and appends a dlq row for observability --
// retry is never aborted on failure.
func (w *Worker) attempt(ctx context.Context, cb store.Callback) Result {
	tgt, err := w.store.GetTarget(ctx, cb.ReceiptID)
	if err != nil {
		w.logger.Error("callback attempt: load target", "receipt", cb.ReceiptID, "err", err)
		return w.fail(ctx, cb, 0, err)
	}
	status, err := w.post(ctx, cb.URL, tgt.Receipt)
	if err == nil && status >= 200 && status < 300 {
		at := w.clock()
		if err := w.store.MarkDone(ctx, cb.ID, cb.ReceiptID, at); err != nil {
			w.logger.Error("callback attempt: mark done", "receipt", cb.ReceiptID, "err", err)
			return Result{CallbackID: cb.ID, Status: status, Err: err}
		}
		return Result{CallbackID: cb.ID, Status: status, Success: true}
	}
	return w.fail(ctx, cb, status, err)
}

// fail records a failed attempt: bumps attempts, schedules the next attempt
// RetryInterval ahead, and appends a dlq row. It returns the failed Result.
func (w *Worker) fail(ctx context.Context, cb store.Callback, status int, cause error) Result {
	now := w.clock()
	attempts := cb.Attempts + 1
	next := now.Add(w.retry)
	if err := w.store.MarkFailed(ctx, cb.ID, next, attempts); err != nil {
		w.logger.Error("callback fail: mark failed", "callback", cb.ID, "err", err)
		return Result{CallbackID: cb.ID, Status: status, Err: cause}
	}
	dlqErr := describeFailure(status, cause)
	if err := w.store.RecordDLQ(ctx, cb.ID, dlqErr, now, attempts); err != nil {
		// Observability-only: a dlq write failure must not change retry.
		w.logger.Error("callback fail: record dlq", "callback", cb.ID, "err", err)
	}
	return Result{CallbackID: cb.ID, Status: status, Err: cause}
}

// describeFailure renders the cause into a short dlq last_error string.
func describeFailure(status int, cause error) string {
	if cause != nil {
		// An SSRF redirect block surfaces as ErrSSRFBlocked; keep it legible.
		if errors.Is(cause, ErrSSRFBlocked) {
			return fmt.Sprintf("blocked: %v", cause)
		}
		return cause.Error()
	}
	return fmt.Sprintf("http %d", status)
}

// post issues the receipt-JSON POST through the SSRF-guarded client. Redirect
// targets are re-validated by CheckRedirect. The body is a snapshot of the
// receipt (the same fields GET /1/receipts/{receipt}.json exposes).
func (w *Worker) post(ctx context.Context, url string, rc store.Receipt) (int, error) {
	body := receiptBody(rc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pushfree-callback/1.0")
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}

// receiptBody marshals the receipt snapshot posted to callback_url. It mirrors
// the GET /1/receipts/{receipt}.json fields so the receiver gets the same
// view a poll would, keyed by the receipt id.
func receiptBody(rc store.Receipt) []byte {
	m := map[string]any{
		"receipt":                rc.ID,
		"status":                 1,
		"acknowledged":           bit(rc.AcknowledgedAt != nil),
		"acknowledged_at":        unixOrNil(rc.AcknowledgedAt),
		"acknowledged_by":        strOrNull(rc.AcknowledgedBy),
		"acknowledged_by_device": strOrNull(rc.AcknowledgedByDevice),
		"delivered":              bit(rc.LastDeliveredAt != nil),
		"delivered_at":           unixOrNil(rc.LastDeliveredAt),
		"expired":                bit(rc.State == "expired" || rc.ExpiredAt != nil),
		"expires_at":             unixOrNil(rc.ExpiresAt),
		"called_back":            bit(rc.CalledBackAt != nil),
		"called_back_at":         unixOrNil(rc.CalledBackAt),
	}
	b, _ := json.Marshal(m)
	return b
}

// Run is the production loop: it ticks at PollInterval and processes due
// callbacks until ctx is canceled. It is intended to be started as a goroutine
// by server wiring; tests do not use it (they drive ProcessDue directly under
// an injected clock).
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := w.ProcessDue(ctx); err != nil {
				w.logger.Error("callback run: process due", "err", err)
			}
		}
	}
}

// hostKey extracts the per-host:port key used for the concurrency semaphore.
// On a parse error it falls back to the raw URL so one bad URL still gets its
// own semaphore (never shared with a valid host).
func hostKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}

// --- small local helpers (kept here so this file owns its formatting) -------

func bit(b bool) int {
	if b {
		return 1
	}
	return 0
}

func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Unix()
}

func strOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}
