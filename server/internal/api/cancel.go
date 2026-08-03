package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pushfree/pushfree/internal/receipts"
	"github.com/pushfree/pushfree/internal/store"
)

// This file is the GREEN phase of todo 24: the Pushover-compatible cancel +
// cancel_by_tag HTTP surface. It is a NEW file; the handlers live on a
// standalone *CancelAPI group (not *Accounts) so they can be wired and
// committed independently of the account/app routes, avoiding cross-worker
// churn on accounts.go (which todo 23 is concurrently editing). The group
// shares the package-level envelope helpers (writeJSON / writeRequestErrors)
// and the app-token format check (userKeyRe) with the rest of the /1/* surface.
//
// API-COMPAT: per EB/A1-pushover-api.md and plan todo 24, cancel is a sibling
// of acknowledge.json on the /1/receipts/{receipt}/ path:
//
//	POST /1/receipts/{receipt}/cancel.json        cancel one queued receipt
//	POST /1/receipts/cancel_by_tag.json           cancel all pending receipts with a tag
//
// cancel_by_tag takes the tag in the form body (not the path) because Go 1.22's
// ServeMux does not permit a suffix on a wildcard segment ({tag}.json is
// rejected), and a bare {tag} segment would conflict with the {receipt}/cancel.json
// pattern. This minor deviation from Pushover's literal URL is recorded for
// docs/API-COMPAT.md.
//
// Semantics: cancel succeeds iff the receipt is still pending (queued, not-yet-
// delivered). A delivered/acknowledged/expired/canceled receipt yields
// {"status":0,"errors":["receipt is <state> and cannot be canceled"]} with HTTP
// 409. On success the retry scheduler observes the terminal canceled state and
// emits EventDone (no further delivery attempts), and the receipt's timers are
// removed.

// CancelBroadcaster is the seam that notifies live transports a receipt was
// canceled, so connected clients stop presenting the alert. It is invoked
// best-effort after a successful cancel; the concrete hub implementation
// (broadcasting a {"type":"cancel",...} frame) is wired once live message push
// exists (todos 22/23 drive the delivery push). nil (the default) means the
// cancel path skips the broadcast -- the persisted canceled state still stops
// retries regardless.
type CancelBroadcaster interface {
	NotifyCanceled(receiptID, tag string)
}

// CancelAPI is the cancel + cancel_by_tag HTTP handler group (todo 24). It is
// constructed and registered separately from *Accounts; server wiring (or a
// test) mounts it on the same ServeMux.
type CancelAPI struct {
	repos       store.Repos
	cancelStore receipts.CancelStore
	broadcaster CancelBroadcaster // nil -> skip live cancel notification
	logger      *slog.Logger
}

// NewCancelAPI builds the cancel handler group. cancelStore is the persistence
// surface (typically internal/store/sqlite.NewCancelRepo); broadcaster is
// optional (nil skips the live cancel notification). A nil logger is replaced
// with a default so handlers never nil-deref.
func NewCancelAPI(repos store.Repos, cancelStore receipts.CancelStore, broadcaster CancelBroadcaster, logger *slog.Logger) *CancelAPI {
	if logger == nil {
		logger = slog.Default()
	}
	return &CancelAPI{repos: repos, cancelStore: cancelStore, broadcaster: broadcaster, logger: logger}
}

// Register mounts the two cancel routes on mux. They are registered with
// Go 1.22 method-patterns so PathValue extracts the receipt id.
func (c *CancelAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /1/receipts/{receipt}/cancel.json", c.cancelHandler)
	mux.HandleFunc("POST /1/receipts/cancel_by_tag.json", c.cancelByTagHandler)
}

// cancelHandler implements POST /1/receipts/{receipt}/cancel.json.
func (c *CancelAPI) cancelHandler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	if err := r.ParseForm(); err != nil {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "could not parse form data")
		return
	}
	app, ok := resolveAppToken(r.Context(), c.repos.Apps, r.FormValue("token"))
	if !ok {
		writeRequestErrors(w, http.StatusUnauthorized, requestID, "application token is invalid")
		return
	}
	receiptID := r.PathValue("receipt")
	rc, err := c.repos.Receipts.GetByID(r.Context(), receiptID)
	if err != nil || !receiptOwnedByApp(r.Context(), c.repos.Sends, rc, app.ID) {
		// Unknown or foreign receipt: 404 with no leakage (a missing receipt
		// is indistinguishable from one owned by another app).
		writeRequestErrors(w, http.StatusNotFound, requestID, "receipt not found")
		return
	}
	ok, err = receipts.Cancel(r.Context(), c.cancelStore, rc.ID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, receipts.ErrNotCancellable) {
			writeRequestErrors(w, http.StatusConflict, requestID,
				fmt.Sprintf("receipt is %s and cannot be canceled", rc.State))
			return
		}
		c.logger.Error("cancel: persist", "receipt", rc.ID, "err", err)
		writeRequestErrors(w, http.StatusInternalServerError, requestID, "could not cancel receipt")
		return
	}
	_ = ok
	// Best-effort: tell live transports to stop presenting the alert. Nil-safe;
	// a broadcaster failure has no HTTP-visible effect (the cancel already
	// persisted).
	if c.broadcaster != nil {
		c.broadcaster.NotifyCanceled(rc.ID, rc.Tag)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 1, "request": requestID})
}

// cancelByTagHandler implements POST /1/receipts/cancel_by_tag.json {tag,token}.
// It cancels every pending receipt with that tag owned by the authenticated
// app and returns their ids in "canceled". A tag matching no pending receipts
// is a 200 with an empty list (idempotent no-op, not an error).
func (c *CancelAPI) cancelByTagHandler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()
	if err := r.ParseForm(); err != nil {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "could not parse form data")
		return
	}
	app, ok := resolveAppToken(r.Context(), c.repos.Apps, r.FormValue("token"))
	if !ok {
		writeRequestErrors(w, http.StatusUnauthorized, requestID, "application token is invalid")
		return
	}
	tag := r.FormValue("tag")
	if tag == "" {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "tag is required")
		return
	}
	canceled, err := receipts.CancelByTag(r.Context(), c.cancelStore, tag, app.ID, time.Now().UTC())
	if err != nil {
		c.logger.Error("cancel_by_tag", "tag", tag, "err", err)
		writeRequestErrors(w, http.StatusInternalServerError, requestID, "could not cancel by tag")
		return
	}
	if c.broadcaster != nil {
		for _, id := range canceled {
			c.broadcaster.NotifyCanceled(id, tag)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   1,
		"request":  requestID,
		"canceled": canceled,
	})
}

// resolveAppToken validates an app token and returns the owning App. A
// malformed, unknown, or revoked (deleted) token yields ok=false; the caller
// maps that to 401. It mirrors Accounts.ValidateAppToken but returns the App
// (so the cancel handlers can scope by app id) rather than just the user id.
func resolveAppToken(ctx context.Context, apps store.AppRepo, token string) (store.App, bool) {
	// userKeyRe is ^[A-Za-z0-9]{30}$ -- the exact app-token spec, shared with
	// the rest of the /1/* surface. A malformed token skips the DB lookup.
	if token == "" || !userKeyRe.MatchString(token) {
		return store.App{}, false
	}
	app, err := apps.GetByToken(ctx, token)
	if err != nil {
		return store.App{}, false
	}
	return app, true
}

// receiptOwnedByApp reports whether receipt belongs to a send created by appID
// (receipt.send_id -> send.app_id). Any lookup miss yields false so a foreign
// or unknown receipt is indistinguishable from a missing one (no enumeration).
func receiptOwnedByApp(ctx context.Context, sends store.SendRepo, rc store.Receipt, appID int64) bool {
	sd, err := sends.GetByID(ctx, rc.SendID)
	if err != nil {
		return false
	}
	return sd.AppID == appID
}
