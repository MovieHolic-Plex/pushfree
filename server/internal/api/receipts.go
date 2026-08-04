package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pushfree/pushfree/internal/store"
)

// This file is the GREEN phase of todo 23: the Pushover-compatible receipt
// ack + lookup HTTP surface, the GC sweeper, and the callback hook SEAM. It
// is a NEW file; the handlers are methods on *Accounts so they share the
// app-token validation helper (ValidateAppToken), the store Repos, and the
// writeJSON/writeRequestErrors envelope helpers with the rest of the /1/*
// surface (messages.json, validate.json, sounds.json).
//
// SPEC NOTE (API-COMPAT): the authoritative Pushover contract (EB/A1-
// pushover-api.md "ack via POST .../acknowledge.json") and plan todos 23/33
// place ack at POST /1/receipts/{receipt}/acknowledge.json, NOT at
// /1/messages/{message_id}/ack.json. cancel.json (todo 24) is a sibling on
// the same /1/receipts/{receipt}/ path. This deviation from a stray task-prompt
// wording is recorded for docs/API-COMPAT.md.

// AckHook is the seam todo 25 (the callback worker) plugs into. It is invoked
// after a receipt transitions to acknowledged so the callback worker can
// enqueue the receipt-JSON webhook POST (retried every 1 min on non-2xx, per
// EB/A1). This task does NOT implement callbacks: the default is nil (no-op),
// and OnAcknowledged errors are logged but NEVER fail the ack, because the
// receipt is already acknowledged and callbacks are a best-effort side effect.
type AckHook interface {
	OnAcknowledged(ctx context.Context, receiptID string) error
}

// SetAckHook installs the callback hook fired after a successful ack (todo 25
// seam). It is a setter (not a constructor param) so server wiring can install
// the callback worker after the Accounts group is built without changing the
// constructor signature owned by todo 6/server wiring. nil (the default) means
// the ack path skips the callback entirely.
func (a *Accounts) SetAckHook(h AckHook) { a.ackHook = h }

// RetrySeeder seeds the first priority-2 retry timer after a successful
// ingest. It is a seam so the api package does not import the timers package
// directly. nil (the default) means no retry timer is created and the receipt
// stays pending (the pre-wiring behavior).
type RetrySeeder interface {
	// SeedRetry creates the initial "retry" timer for receiptID, using the
	// caller-supplied retry interval (seconds) and expire window (seconds).
	// createdAt is t=0 of the retry schedule. The timer fires immediately
	// (the first attempt is due at createdAt).
	SeedRetry(ctx context.Context, receiptID string, retryInterval, expireSeconds int, createdAt time.Time) error
}

// SetRetrySeeder installs the priority-2 retry seeder (todo 21/22 wiring).
// nil (the default) means priority-2 receipts are created but never retried.
func (a *Accounts) SetRetrySeeder(s RetrySeeder) { a.retrySeeder = s }

// --- GET /1/receipts/{receipt}.json ----------------------------------------

// receiptHandler implements receipt polling: GET /1/receipts/{receipt}.json
// ?token=<app token>. It returns the A1 snapshot fields (acknowledged/_at/
// _by/_by_device, delivered_at, expired, expires_at, called_back/_at). Polling
// cadence is client-driven (>=5s guidance is a client concern); the server
// only answers. Auth + ownership: the token must belong to the user whose app
// created the receipt's send; a foreign or unknown token yields 404 (no
// cross-user enumeration), matching Pushover's "not found" for a receipt that
// is not yours.
func (a *Accounts) receiptHandler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	token := r.URL.Query().Get("token")
	uid, err := a.ValidateAppToken(r.Context(), token)
	if err != nil {
		writeRequestErrors(w, http.StatusUnauthorized, requestID, "application token is invalid")
		return
	}
	import_mirror := 0
	_ = import_mirror
	id := strings.TrimSuffix(r.PathValue("receipt"), ".json")
	rc, err := a.repos.Receipts.GetByID(r.Context(), id)
	if err != nil || !a.receiptOwnedBy(r.Context(), rc, uid) {
		writeRequestErrors(w, http.StatusNotFound, requestID, "receipt not found")
		return
	}
	writeJSON(w, http.StatusOK, a.receiptSnapshot(requestID, rc))
}

// --- POST /1/receipts/{receipt}/acknowledge.json ---------------------------

// ackHandler acknowledges a receipt. Auth accepts EITHER the recipient's
// device (device_id + secret, hashed exactly as /1/devices/login.json stores
// it) OR the owning application token. Possession of a valid device secret
// plus the unguessable 30-char receipt id is the capability to ack (the device
// received the emergency alert out-of-band); an unknown device or wrong secret
// yields 401. On success the response is the post-ack receipt snapshot, so a
// single round-trip shows acknowledged. The callback hook (todo 25) is fired
// best-effort after the transition.
func (a *Accounts) ackHandler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	if err := r.ParseForm(); err != nil {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "could not parse form data")
		return
	}
	receiptID := r.PathValue("receipt")
	rc, err := a.repos.Receipts.GetByID(r.Context(), receiptID)
	if err != nil {
		writeRequestErrors(w, http.StatusNotFound, requestID, "receipt not found")
		return
	}

	var ackBy, ackByDevice string
	token := r.FormValue("token")
	deviceID := r.FormValue("device_id")
	secret := r.FormValue("secret")
	switch {
	case token != "":
		// App-token path: the token must own this receipt's send.
		uid, err := a.ValidateAppToken(r.Context(), token)
		if err != nil || !a.receiptOwnedBy(r.Context(), rc, uid) {
			writeRequestErrors(w, http.StatusUnauthorized, requestID, "application token is invalid")
			return
		}
		u, err := a.repos.Users.GetByID(r.Context(), uid)
		if err != nil {
			writeRequestErrors(w, http.StatusUnauthorized, requestID, "application token is invalid")
			return
		}
		ackBy = u.UserKey
	case deviceID != "" || secret != "":
		// Device-secret path: the recipient's device acknowledges.
		dev, ok := authenticateAckDevice(r.Context(), a.repos.Devices, deviceID, secret)
		if !ok {
			writeRequestErrors(w, http.StatusUnauthorized, requestID, "invalid device_id or secret")
			return
		}
		u, err := a.repos.Users.GetByID(r.Context(), dev.UserID)
		if err != nil {
			writeRequestErrors(w, http.StatusUnauthorized, requestID, "invalid device_id or secret")
			return
		}
		ackBy = u.UserKey
		ackByDevice = dev.Name
	default:
		writeRequestErrors(w, http.StatusUnauthorized, requestID, "ack requires a device secret or application token")
		return
	}

	// Acknowledge is idempotent + an illegal-forward no-op on terminal rows,
	// so re-acks and races with expiry are safe. at is UTC for stable storage.
	at := time.Now().UTC()
	if err := a.repos.Receipts.Acknowledge(r.Context(), rc.ID, ackBy, ackByDevice, at); err != nil {
		a.logger.Error("ack: acknowledge", "receipt", rc.ID, "err", err)
		writeRequestErrors(w, http.StatusInternalServerError, requestID, "could not acknowledge receipt")
		return
	}

	// Callback hook seam (todo 25). Best-effort: the receipt is already
	// acknowledged, so a hook failure must not fail the HTTP response.
	if a.ackHook != nil {
		if err := a.ackHook.OnAcknowledged(r.Context(), rc.ID); err != nil {
			a.logger.Error("ack: callback hook (todo 25 seam)", "receipt", rc.ID, "err", err)
		}
	}

	// Re-read so the response reflects persisted state (preserves the original
	// acknowledged_at/_by on an idempotent re-ack rather than echoing `at`).
	rc2, err := a.repos.Receipts.GetByID(r.Context(), rc.ID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": 1, "request": requestID})
		return
	}
	writeJSON(w, http.StatusOK, a.receiptSnapshot(requestID, rc2))
}

// receiptOwnedBy reports whether uid owns the app that created the receipt's
// send (receipt.send_id -> send.app_id -> app.user_id). Any lookup miss yields
// false so a foreign/unknown receipt is indistinguishable from a missing one
// (no enumeration).
func (a *Accounts) receiptOwnedBy(ctx context.Context, rc store.Receipt, uid int64) bool {
	sd, err := a.repos.Sends.GetByID(ctx, rc.SendID)
	if err != nil {
		return false
	}
	app, err := a.repos.Apps.GetByID(ctx, sd.AppID)
	if err != nil {
		return false
	}
	return app.UserID == uid
}

// receiptSnapshot builds the A1 receipt-fields response from a stored receipt.
// Boolean fields use 0/1 (Pushover convention); absent timestamps/keys are null
// (not omitted) so a client can distinguish "not yet" from "unknown".
func (a *Accounts) receiptSnapshot(requestID string, rc store.Receipt) map[string]any {
	return map[string]any{
		"status":                 1,
		"request":                requestID,
		"acknowledged":           receiptBit(rc.AcknowledgedAt != nil),
		"acknowledged_at":        unixOrNil(rc.AcknowledgedAt),
		"acknowledged_by":        strOrNull(rc.AcknowledgedBy),
		"acknowledged_by_device": strOrNull(rc.AcknowledgedByDevice),
		"delivered":              receiptBit(rc.LastDeliveredAt != nil),
		"delivered_at":           unixOrNil(rc.LastDeliveredAt),
		"expired":                receiptBit(rc.State == "expired" || rc.ExpiredAt != nil),
		"expires_at":             unixOrNil(rc.ExpiresAt),
		"called_back":            receiptBit(rc.CalledBackAt != nil),
		"called_back_at":         unixOrNil(rc.CalledBackAt),
	}
}

// authenticateAckDevice validates a (device_id, secret) pair against the stored
// SHA-256 hash, mirroring hub.authenticateDevice. Duplicated here (not imported
// from hub) so the api package does not depend on hub internals and the two
// packages can be wired independently. An unknown device, empty credential, or
// hash mismatch all yield ok=false.
func authenticateAckDevice(ctx context.Context, devs store.DeviceRepo, deviceID, secret string) (store.Device, bool) {
	if deviceID == "" || secret == "" {
		return store.Device{}, false
	}
	dev, err := devs.GetByDeviceID(ctx, deviceID)
	if err != nil {
		return store.Device{}, false
	}
	sum := sha256.Sum256([]byte(secret))
	if hex.EncodeToString(sum[:]) != dev.SecretHash {
		return store.Device{}, false
	}
	return dev, true
}

// unixOrNil returns the epoch-second timestamp for a nullable instant, or nil
// (JSON null) when absent. A nil pointer is the "not yet" state for every
// receipt *_at field.
func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Unix()
}

// strOrNull returns s, or nil (JSON null) when s is empty -- the storage
// convention that "" means NULL for optional TEXT columns.
func strOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// receiptBit returns 1 when b is true, else 0 -- the Pushover 0/1 convention for
// the boolean receipt snapshot fields (acknowledged/delivered/expired/
// called_back). Kept local so this file does not depend on a shared helper.
func receiptBit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// --- Receipt GC sweeper -----------------------------------------------------

// ReceiptGCStore is the narrow persistence surface the ReceiptSweeper needs.
// store.ReceiptRepo satisfies it (it carries SweepReceipts from todo 23).
type ReceiptGCStore interface {
	SweepReceipts(ctx context.Context, now time.Time, retention time.Duration) (store.ReceiptSweepResult, error)
}

// ReceiptSweeper is the background GC for the 7-day receipt retention window
// (the Pushover receipt query window). now and interval are injectable so the
// retention boundary is tested without sleeping; Sweep is the testable unit and
// Run is the ticker loop that server wiring (todo 41/later) starts as a
// goroutine. A retention <= 0 defaults to 7 days; a nil now defaults to
// time.Now; an interval <= 0 defaults to 1 hour.
type ReceiptSweeper struct {
	receipts  ReceiptGCStore
	retention time.Duration
	now       func() time.Time
	interval  time.Duration
}

// NewReceiptSweeper builds a sweeper. receipts must be non-nil.
func NewReceiptSweeper(receipts ReceiptGCStore, retention time.Duration, now func() time.Time, interval time.Duration) *ReceiptSweeper {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &ReceiptSweeper{receipts: receipts, retention: retention, now: now, interval: interval}
}

// Sweep runs one GC pass at the injected clock's current instant and returns
// the per-table delete counts. It is safe to call directly (the tests do) and
// is what Run invokes on each tick.
func (s *ReceiptSweeper) Sweep(ctx context.Context) (store.ReceiptSweepResult, error) {
	now := s.now
	if now == nil {
		now = time.Now
	}
	return s.receipts.SweepReceipts(ctx, now(), s.retention)
}

// Run is the background loop: it sweeps immediately, then every interval until
// ctx is canceled. It is intended to be started as a goroutine by server
// wiring; the live goroutine is out of this task's scope (todo 25/server
// wiring owns startup ordering).
func (s *ReceiptSweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	// One immediate pass so a restart reclaims expired receipts promptly
	// rather than waiting up to one interval.
	if _, err := s.Sweep(ctx); err != nil {
		// Swallowed on the background path: a transient DB error is retried on
		// the next tick. (Sweep errors are asserted in the direct-call tests.)
		_ = err
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Sweep(ctx); err != nil {
				_ = err
			}
		}
	}
}
