package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/pushfree/pushfree/internal/store"
)

// appPublic is the JSON shape returned by GET /1/apps. The token is included
// verbatim because the owner needs to copy it into integrations.
type appPublic struct {
	ID    int64  `json:"id"`
	Token string `json:"token"`
	Name  string `json:"name"`
}

// createApp handles POST /1/apps (session-auth). Body: {"name":"..."}.
// Response: 200 {"status":1,"token":"<30-char [A-Za-z0-9]>"}.
func (a *Accounts) createApp(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with name")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErrors(w, http.StatusBadRequest, "name is required")
		return
	}
	// 30-char [A-Za-z0-9] from crypto/rand (newAppToken). Collisions on a
	// 62^30 space are astronomically unlikely; retry on a UNIQUE violation
	// rather than surfacing the collision to the caller.
	var token string
	for attempt := 0; attempt < 3; attempt++ {
		tok, err := newAppToken()
		if err != nil {
			a.logger.Error("create app: generate token", "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not create app")
			return
		}
		_, err = a.repos.Apps.Create(r.Context(), &store.App{UserID: uid, Token: tok, Name: name})
		if err == nil {
			token = tok
			break
		}
		if !store.IsUniqueViolation(err) {
			a.logger.Error("create app: insert", "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not create app")
			return
		}
	}
	if token == "" {
		a.logger.Error("create app: token allocation exhausted after retries")
		writeErrors(w, http.StatusInternalServerError, "could not allocate app token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": 1, "token": token})
}

// listApps handles GET /1/apps (session-auth). The all=1 query param is the
// documented mode; the response always lists every app owned by the caller
// with its token value (there is no subset concept yet).
func (a *Accounts) listApps(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	apps, err := a.repos.Apps.ListByUser(r.Context(), uid)
	if err != nil {
		a.logger.Error("list apps", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not list apps")
		return
	}
	out := make([]appPublic, 0, len(apps))
	for _, ap := range apps {
		out = append(out, appPublic{ID: ap.ID, Token: ap.Token, Name: ap.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 1, "apps": out})
}

// deleteApp handles DELETE /1/apps/{token} (session-auth). Only the owner can
// revoke: DeleteByToken scopes on (userID, token), so a token that does not
// belong to the caller returns 404 (no cross-user enumeration). Revoking a
// token makes it fail ValidateAppToken, which the send path (todo 8) maps to
// 401. Response: 200 {"status":1}.
func (a *Accounts) deleteApp(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	token := r.PathValue("token")
	if token == "" {
		writeErrors(w, http.StatusNotFound, "app not found")
		return
	}
	if err := a.repos.Apps.DeleteByToken(r.Context(), uid, token); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusNotFound, "app not found")
			return
		}
		a.logger.Error("delete app", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not revoke app")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 1})
}

// ValidateAppToken resolves an app token to its owning user id. It is the
// shared auth check for the Pushover-compatible send path (todo 8
// messages.json) and for limitWrap's query-token resolution. A malformed,
// unknown, or revoked (deleted) token yields ErrInvalidAppToken; the caller
// maps that to 401 via WriteInvalidAppToken. A malformed token is rejected
// without a database lookup.
func (a *Accounts) ValidateAppToken(ctx context.Context, token string) (int64, error) {
	// userKeyRe is ^[A-Za-z0-9]{30}$ -- the exact app-token spec.
	if token == "" || !userKeyRe.MatchString(token) {
		return 0, ErrInvalidAppToken
	}
	app, err := a.repos.Apps.GetByToken(ctx, token)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, ErrInvalidAppToken
		}
		return 0, err
	}
	return app.UserID, nil
}

// WriteInvalidAppToken writes the canonical send-path 401 body for an invalid
// app token: {"status":0,"errors":["application token is invalid"]}. Exported
// so todo 8's messages.json handler reuses the exact error string rather than
// re-literalizing it.
func WriteInvalidAppToken(w http.ResponseWriter) {
	writeErrors(w, http.StatusUnauthorized, "application token is invalid")
}
