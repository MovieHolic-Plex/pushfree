package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pushfree/pushfree/internal/quota"
)

// monthlyLimit is the per-user send quota surfaced verbatim in
// X-Limit-App-Limit. It mirrors the config.quota-monthly default (todo 4).
// The value is a constant here because the Accounts constructor signature is
// owned by server wiring (todo 6) and cannot grow a parameter without
// touching internal/server/server.go; wiring the configured value is a
// server-wiring concern (todo 41).
const monthlyLimit = 10000

// ErrInvalidAppToken is returned by ValidateAppToken when a token is
// malformed, unknown, or revoked. The send path (todo 8 messages.json) maps
// it to 401 {"status":0,"errors":["application token is invalid"]} via
// WriteInvalidAppToken.
var ErrInvalidAppToken = errors.New("api: application token is invalid")

// --- Monthly quota reset zone (todo 10) ------------------------------------
// The period/reset window is America/Chicago (Central Time): the quota resets
// at 00:00:00 CT on the 1st of each month, DST-aware. The pure time math lives
// in internal/quota (Period/NextReset over an injected *time.Location) so it
// is unit-tested at the month boundary with a fixed clock; these wrappers keep
// the existing applimit.go callers on the same Central-Time zone.
//
// The counter lives in quota_counters via store.QuotaRepo; todo 8 charges it
// post-ingest (MarkSendAccepted / the messages.json Increment), and todo 10
// adds the pre-write 429 gate (internal/api/quota.go + messages.go) and the
// GET /1/apps/limits.json endpoint. Delivery retries never re-count (the
// regression note stays in todo 26).

// quotaPeriod returns the "YYYY-MM" billing period containing now, evaluated
// in America/Chicago (Central Time). DST-safe: the month is computed in the
// CT zone, not in UTC.
func quotaPeriod(now time.Time) string {
	return quota.Period(quota.CentralTime, now)
}

// nextMonthReset returns the epoch-second timestamp of the next America/Chicago
// calendar-month boundary (the first instant 00:00:00 CT of the following
// month). It is the value reported in X-Limit-App-Reset and limits.json's
// "reset". During CDT (summer) that is 05:00 UTC; during CST (winter) 06:00 UTC.
func nextMonthReset(now time.Time) int64 {
	return quota.NextReset(quota.CentralTime, now)
}

// quotaSnapshot returns (limit, remaining, resetEpochSeconds) for userID in
// the current period. A zero userID (unresolved caller) yields the full
// quota without a database read. On a counter read failure the header fails
// open (full quota): the header is informational and todo 8/10 enforce the
// real limit at send-acceptance time.
func (a *Accounts) quotaSnapshot(ctx context.Context, userID int64) (limit, remaining int, reset int64) {
	limit = monthlyLimit
	reset = nextMonthReset(time.Now())
	if userID == 0 {
		return limit, limit, reset
	}
	qc, err := a.repos.Quota.Get(ctx, userID, quotaPeriod(time.Now()))
	if err != nil {
		a.logger.Error("quota snapshot: get counter", "user_id", userID, "err", err)
		return limit, limit, reset
	}
	remaining = limit - int(qc.Count)
	if remaining < 0 {
		remaining = 0
	}
	return limit, remaining, reset
}

// SetLimitHeaders writes the three X-Limit-App-* response headers for userID.
// It is exported so handlers that resolve the caller from the request body
// (todo 8 messages.json reads the token from the form body) can set the
// headers AFTER parsing the body, overriding whatever the middleware wrote
// from the query string. Must be called before WriteHeader.
func (a *Accounts) SetLimitHeaders(w http.ResponseWriter, userID int64) {
	limit, remaining, reset := a.quotaSnapshot(context.Background(), userID)
	w.Header().Set("X-Limit-App-Limit", strconv.Itoa(limit))
	w.Header().Set("X-Limit-App-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-Limit-App-Reset", strconv.FormatInt(reset, 10))
}

// MarkSendAccepted records one accepted send against the caller's monthly
// quota and returns the post-increment remaining count (clamped at 0). todo
// 8's messages.json handler MUST call this after a send passes validation and
// BEFORE writing the success response, then call SetLimitHeaders(w, userID)
// so X-Limit-App-Remaining reflects the just-accepted send. It does not
// enforce the limit itself; todo 10 adds 429 enforcement.
func (a *Accounts) MarkSendAccepted(ctx context.Context, userID int64) (remaining int, err error) {
	count, err := a.repos.Quota.Increment(ctx, userID, quotaPeriod(time.Now()), 1)
	if err != nil {
		return 0, err
	}
	remaining = monthlyLimit - int(count)
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

// limitWrap is the /1/* response middleware: it resolves the caller's user
// id (session cookie first, then ?token= query) and writes the X-Limit-App-*
// headers before delegating to next. Session resolution reuses parseSession
// (set by requireSession when present); token resolution reuses
// ValidateAppToken so a revoked token yields no user (full-quota headers)
// and the wrapped handler returns its own 401. Body-token send routes (todo
// 8) that resolve the user from the form should additionally call
// SetLimitHeaders after parsing, so the body token's quota is reported.
func (a *Accounts) limitWrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid, _ := getUserID(r.Context())
		if uid == 0 {
			if tok := r.URL.Query().Get("token"); tok != "" {
				if id, err := a.ValidateAppToken(r.Context(), tok); err == nil {
					uid = id
				}
			}
		}
		a.SetLimitHeaders(w, uid)
		next(w, r)
	}
}
