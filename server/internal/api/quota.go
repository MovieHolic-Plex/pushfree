package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/pushfree/pushfree/internal/quota"
)

// quotaManager returns a quota.Manager bound to the caller's store, the
// configured monthly limit (applimit.go's monthlyLimit constant), and the
// Central-Time reset zone. The clock is wall time; the quota package itself
// is where a fixed clock is injected for its own month-boundary tests.
//
// Constructed per-call rather than cached on *Accounts because the Accounts
// constructor signature is owned by server wiring (main.go) and cannot grow a
// field without touching it; the Manager is a few-field value, cheap to build.
func (a *Accounts) quotaManager() *quota.Manager {
	return quota.NewManager(a.repos.Quota, monthlyLimit, quota.CentralTime, nil)
}

// prechargeQuota is the PRE-WRITE gate (plan todo 10): it reports whether
// senderID may send n more messages this period WITHOUT persisting anything.
// It charges per concrete recipient (n == len(resolved recipient IDs)), so a
// group send with N members costs N -- consistent with the post-ingest
// Increment in messages.json. On a store read error it fails CLOSED
// (allowed=false) so a transient outage cannot let quota escape. The returned
// reset epoch lets the caller populate X-Limit-App-Reset on the refusal.
func (a *Accounts) prechargeQuota(ctx context.Context, senderID, n int64) (allowed bool, reset int64) {
	allowed, _, reset, err := a.quotaManager().Allow(ctx, senderID, n)
	if err != nil {
		a.logger.Error("quota gate: read counter", "user_id", senderID, "recipients", n, "err", err)
		return false, reset
	}
	return allowed, reset
}

// limitsHandler implements GET /1/apps/limits.json (plan todo 10). Given a
// valid app token (query param "token") it returns the owner's monthly quota
// usage as exactly:
//
//	{"count":N,"limit":10000,"remaining":M,"reset":E}
//
// The quota is PER-USER and shared across all of the user's apps; the token
// only identifies the owner. count is the sends already charged this period,
// limit is the monthly cap, remaining is limit-count (clamped at 0), and reset
// is the epoch second of the next America/Chicago month boundary. A missing or
// invalid token yields 400 (the Pushover convention for these metadata
// endpoints, matching sounds.json/validate.json). The route is mounted with
// limitWrap so the response also carries the X-Limit-App-* headers.
func (a *Accounts) limitsHandler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()

	token := r.URL.Query().Get("token")
	uid, err := a.ValidateAppToken(r.Context(), token)
	if err != nil {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "application token is invalid")
		return
	}

	count, limit, remaining, reset, qerr := a.quotaManager().Snapshot(r.Context(), uid)
	if qerr != nil {
		a.logger.Error("limits.json: snapshot", "user_id", uid, "err", qerr)
		writeRequestErrors(w, http.StatusInternalServerError, requestID, "could not read quota")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":     count,
		"limit":     limit,
		"remaining": remaining,
		"reset":     reset,
	})
}
