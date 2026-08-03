package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// Subscription codes with dynamic per-app keys (todo 12). These handlers are
// methods on *Accounts so they share the app-token validation helper
// (ValidateAppToken), the session middleware (requireSession), and the JSON
// envelope helpers with the rest of the surface.
//
// Endpoints:
//   - POST /1/subscriptions (token=app token, title) -> {subscription_code,
//     subscribe_url}. Token-authenticated like the rest of the /1/* Pushover-
//     compatible surface; the body is JSON.
//   - POST /1/subscriptions/authorize (session cookie, subscription_code) ->
//     {subscribed_user_key}. Session-authenticated approval (the dashboard UI
//     comes in todo 41). Generates a PER-APP dynamic key: different per app,
//     stable per app+user (re-approving returns the same key).
//   - POST /1/subscriptions/migrate.json (subscription_code, from_app_token,
//     to_app_token) -> re-parents the channel and remaps keys (old invalid,
//     new valid). Token-authenticated.
//
// A subscribed_user_key resolves transparently like a user_key in the send
// path (ResolveRecipients in send_message.go): sending to it delivers one
// message to the underlying user.

// maxSubscriptionTitleRunes caps the subscription channel title (same limit as
// a message title).
const maxSubscriptionTitleRunes = 250

// --- POST /1/subscriptions --------------------------------------------------

type createSubscriptionRequest struct {
	Token string `json:"token"`
	Title string `json:"title"`
}

// createSubscription issues a subscription channel for the app identified by
// the body token. Response: 200 {"status":1,"subscription_code":"<30-char>",
// "subscribe_url":"<path>"}. The subscribe_url is a relative path to the
// future dashboard subscribe page (todo 41); it carries the code so the page
// can call /1/subscriptions/authorize once it exists.
func (a *Accounts) createSubscription(w http.ResponseWriter, r *http.Request) {
	var req createSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with token and title")
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeErrors(w, http.StatusBadRequest, "token is required")
		return
	}
	// A present-but-invalid/revoked token is 401, matching the send path.
	uid, err := a.ValidateAppToken(r.Context(), token)
	if err != nil {
		writeErrors(w, http.StatusUnauthorized, "application token is invalid")
		return
	}
	app, err := a.repos.Apps.GetByToken(r.Context(), token)
	if err != nil {
		writeErrors(w, http.StatusUnauthorized, "application token is invalid")
		return
	}
	title := strings.TrimSpace(req.Title)
	if len([]rune(title)) > maxSubscriptionTitleRunes {
		writeErrors(w, http.StatusBadRequest, "title must be 250 characters or fewer")
		return
	}

	// subscription_code is 30-char [A-Za-z0-9]; collisions on a 62^30 space
	// are astronomically unlikely, but retry on a UNIQUE violation.
	var code string
	for attempt := 0; attempt < 3; attempt++ {
		c, gerr := newSubscriptionCode()
		if gerr != nil {
			a.logger.Error("create subscription: generate code", "err", gerr)
			writeErrors(w, http.StatusInternalServerError, "could not create subscription")
			return
		}
		_, ierr := a.repos.Subscriptions.Create(r.Context(), &store.Subscription{
			AppID:            app.ID,
			OwnerUserID:      uid,
			SubscriptionCode: c,
			Title:            title,
			CreatedAt:        time.Now().UTC(),
		})
		if ierr == nil {
			code = c
			break
		}
		if !store.IsUniqueViolation(ierr) {
			a.logger.Error("create subscription: insert", "err", ierr)
			writeErrors(w, http.StatusInternalServerError, "could not create subscription")
			return
		}
	}
	if code == "" {
		a.logger.Error("create subscription: code allocation exhausted after retries")
		writeErrors(w, http.StatusInternalServerError, "could not allocate subscription code")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            1,
		"subscription_code": code,
		// Relative path: the dashboard (todo 41) hosts the user-facing approve
		// page here. It is non-empty and carries the code.
		"subscribe_url": "/subscribe/" + code,
	})
}

// --- POST /1/subscriptions/authorize ----------------------------------------

type authorizeSubscriptionRequest struct {
	SubscriptionCode string `json:"subscription_code"`
}

// authorizeSubscription is the session-authenticated approval endpoint. The
// logged-in user approves the subscription identified by subscription_code,
// minting (or returning) the per-app+user dynamic key. Response: 200
// {"status":1,"subscribed_user_key":"<30-char>"}.
func (a *Accounts) authorizeSubscription(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	var req authorizeSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with subscription_code")
		return
	}
	code := strings.TrimSpace(req.SubscriptionCode)
	if code == "" || !userKeyRe.MatchString(code) {
		writeErrors(w, http.StatusBadRequest, "subscription_code is invalid")
		return
	}
	sub, err := a.repos.Subscriptions.GetByCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusNotFound, "subscription not found")
			return
		}
		a.logger.Error("authorize subscription: lookup", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not authorize subscription")
		return
	}
	// Mint the per-app+user key. appID is the subscription's current app, so
	// a migrated channel mints keys under the destination app.
	key, err := a.repos.Subscriptions.Approve(r.Context(), sub.ID, sub.AppID, uid)
	if err != nil {
		a.logger.Error("authorize subscription: approve", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not authorize subscription")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":              1,
		"subscribed_user_key": key.SubscribedKey,
	})
}

// --- POST /1/subscriptions/migrate.json -------------------------------------

type migrateSubscriptionRequest struct {
	SubscriptionCode string `json:"subscription_code"`
	FromAppToken     string `json:"from_app_token"`
	ToAppToken       string `json:"to_app_token"`
}

// migrateSubscription re-parents a subscription from one app to another and
// regenerates every subscriber key (old keys invalidated, new keys minted for
// the destination app), preserving each recipient user. Both app tokens must
// resolve and belong to the same owner; the subscription must currently be
// parented on from_app. Response: 200 {"status":1,"migrated":<count>}.
func (a *Accounts) migrateSubscription(w http.ResponseWriter, r *http.Request) {
	var req migrateSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with subscription_code, from_app_token and to_app_token")
		return
	}
	code := strings.TrimSpace(req.SubscriptionCode)
	fromTok := strings.TrimSpace(req.FromAppToken)
	toTok := strings.TrimSpace(req.ToAppToken)
	if code == "" || !userKeyRe.MatchString(code) {
		writeErrors(w, http.StatusBadRequest, "subscription_code is invalid")
		return
	}
	if fromTok == "" || toTok == "" {
		writeErrors(w, http.StatusBadRequest, "from_app_token and to_app_token are required")
		return
	}

	fromApp, err := a.repos.Apps.GetByToken(r.Context(), fromTok)
	if err != nil {
		writeErrors(w, http.StatusUnauthorized, "from_app_token is invalid")
		return
	}
	toApp, err := a.repos.Apps.GetByToken(r.Context(), toTok)
	if err != nil {
		writeErrors(w, http.StatusUnauthorized, "to_app_token is invalid")
		return
	}
	// Both apps must belong to the same owner: only the channel owner may
	// re-parent it, and the destination must be theirs.
	if fromApp.UserID != toApp.UserID {
		writeErrors(w, http.StatusForbidden, "from_app_token and to_app_token must belong to the same account")
		return
	}

	sub, err := a.repos.Subscriptions.GetByCode(r.Context(), code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusNotFound, "subscription not found")
			return
		}
		a.logger.Error("migrate subscription: lookup", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not migrate subscription")
		return
	}
	// The subscription must currently be parented on from_app, and the caller
	// must own the channel.
	if sub.AppID != fromApp.ID {
		writeErrors(w, http.StatusBadRequest, "subscription is not parented on from_app_token")
		return
	}
	if sub.OwnerUserID != fromApp.UserID {
		writeErrors(w, http.StatusForbidden, "subscription does not belong to this account")
		return
	}

	count, err := a.repos.Subscriptions.Migrate(r.Context(), sub.ID, fromApp.ID, toApp.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusNotFound, "subscription not found")
			return
		}
		a.logger.Error("migrate subscription: migrate", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not migrate subscription")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   1,
		"migrated": count,
	})
}

// newSubscriptionCode returns a 30-char [A-Za-z0-9] subscription code,
// format-identical to a user_key (so it is indistinguishable from other keys
// by format alone; resolution is by table lookup).
func newSubscriptionCode() (string, error) { return newUserKey() }
