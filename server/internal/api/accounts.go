package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// Accounts is the account-management HTTP handler group. It is wired onto an
// existing ServeMux via Register; it owns no listener. In addition to the
// /v1/accounts surface it mounts the session-authenticated app-token
// management routes under /v1/apps. It also owns the X-Limit-App-* header
// helpers (applimit.go) that todo 8's /1/* send path attaches to its
// responses, and the app-token validation helper the send path uses for auth.
type Accounts struct {
	repos      store.Repos
	authSecret []byte
	ttl        time.Duration
	logger     *slog.Logger
}

// New builds an Accounts handler group. authSecret signs stateless session
// cookies; ttl is the session lifetime (defaults to sessionTTL when <= 0). A
// nil logger is replaced with a default so handlers never nil-deref.
func New(repos store.Repos, authSecret []byte, ttl time.Duration, logger *slog.Logger) *Accounts {
	if ttl <= 0 {
		ttl = sessionTTL
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Accounts{repos: repos, authSecret: authSecret, ttl: ttl, logger: logger}
}

// Register mounts the account routes on mux. /health and other groups are left
// untouched. It also mounts the session-authenticated app-token management
// routes under /v1/apps, matching the /v1/accounts convention. The
// X-Limit-App-* response headers are NOT applied here: they belong on the
// /1/* send path (todo 8 messages.json), which attaches them via
// SetLimitHeaders / limitWrap from applimit.go.
func (a *Accounts) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/accounts", a.register)
	mux.HandleFunc("POST /v1/accounts/login", a.login)
	mux.HandleFunc("GET /v1/accounts/me", a.requireSession(a.me))
	mux.HandleFunc("PUT /v1/accounts/quiet-hours", a.requireSession(a.quietHours))

	// App-token management (session-auth), same pattern as /v1/accounts*.
	mux.HandleFunc("POST /v1/apps", a.requireSession(a.createApp))
	mux.HandleFunc("GET /v1/apps", a.requireSession(a.listApps))
	mux.HandleFunc("DELETE /v1/apps/{token}", a.requireSession(a.deleteApp))

	// Pushover-compatible send path. limitWrap attaches X-Limit-App-* headers
	// from the resolved caller; messagesHandler re-resolves the caller from
	// the form-body token and calls SetLimitHeaders again so remaining
	// reflects the just-accepted send.
	mux.HandleFunc("POST /1/messages.json", a.limitWrap(a.messagesHandler))
}

// --- JSON helpers -----------------------------------------------------------

// writeJSON writes a 200-ish JSON body. Used only on the success path.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErrors writes a Pushover-style error envelope:
// {"status":0,"errors":["...","..."]}.
func writeErrors(w http.ResponseWriter, status int, errs ...string) {
	writeJSON(w, status, map[string]any{"status": 0, "errors": errs})
}

// --- POST /v1/accounts ------------------------------------------------------

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *Accounts) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with email and password")
		return
	}
	req.Email = normalizeEmail(req.Email)
	var errs []string
	if !validEmail(req.Email) {
		errs = append(errs, "email is invalid")
	}
	if len(req.Password) < 8 {
		errs = append(errs, "password must be at least 8 characters")
	}
	if len(errs) > 0 {
		writeErrors(w, http.StatusBadRequest, errs...)
		return
	}

	userKey, err := newUserKey()
	if err != nil {
		a.logger.Error("register: generate user key", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not create account")
		return
	}
	passHash, err := hashPassword(req.Password)
	if err != nil {
		a.logger.Error("register: hash password", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not create account")
		return
	}

	u := &store.User{
		Email:     req.Email,
		PassHash:  passHash,
		UserKey:   userKey,
		QuietTZ:   "UTC",
		CreatedAt: time.Now().UTC(),
	}
	if _, err := a.repos.Users.CreateBootstrap(r.Context(), u); err != nil {
		if store.IsUniqueViolation(err) {
			writeErrors(w, http.StatusConflict, "email is already registered")
			return
		}
		a.logger.Error("register: create user", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not create account")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": 1, "user_key": userKey})
}

// --- POST /v1/accounts/login ------------------------------------------------

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (a *Accounts) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with email and password")
		return
	}
	// The same generic message is returned for an unknown email and a wrong
	// password to avoid user enumeration.
	u, err := a.repos.Users.GetByEmail(r.Context(), normalizeEmail(req.Email))
	if err != nil {
		writeErrors(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	ok, err := verifyPassword(req.Password, u.PassHash)
	if err != nil || !ok {
		writeErrors(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	expiry := time.Now().Add(a.ttl)
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    signSession(a.authSecret, u.ID, expiry.Unix()),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		Expires:  expiry,
	}
	http.SetCookie(w, cookie)
	writeJSON(w, http.StatusOK, map[string]any{"status": 1})
}

// --- GET /v1/accounts/me ----------------------------------------------------

func (a *Accounts) me(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	u, err := a.repos.Users.GetByID(r.Context(), uid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusUnauthorized, "session invalid or expired")
			return
		}
		a.logger.Error("me: get user", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not load account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   1,
		"email":    u.Email,
		"role":     u.Role,
		"user_key": u.UserKey,
		"quiet_hours": map[string]any{
			"start": u.QuietStart,
			"end":   u.QuietEnd,
			"tz":    u.QuietTZ,
		},
	})
}

// --- PUT /v1/accounts/quiet-hours -------------------------------------------

type quietHoursRequest struct {
	QuietStart string `json:"quiet_start"`
	QuietEnd   string `json:"quiet_end"`
	TZ         string `json:"tz"`
}

func (a *Accounts) quietHours(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	var req quietHoursRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with quiet_start, quiet_end and tz")
		return
	}

	clearing := req.QuietStart == "" && req.QuietEnd == ""
	var errs []string
	if !clearing && (!validHHMM(req.QuietStart) || !validHHMM(req.QuietEnd)) {
		errs = append(errs, "quiet_start and quiet_end must be HH:MM, or both empty to clear")
	}
	if (req.QuietStart != "") != (req.QuietEnd != "") {
		errs = append(errs, "quiet_start and quiet_end must be set together")
	}
	if req.TZ == "" {
		errs = append(errs, "tz is required")
	} else if _, err := time.LoadLocation(req.TZ); err != nil {
		errs = append(errs, "tz is not a valid IANA timezone")
	}
	if len(errs) > 0 {
		writeErrors(w, http.StatusBadRequest, errs...)
		return
	}

	if err := a.repos.Users.UpdateQuietHours(r.Context(), uid, req.QuietStart, req.QuietEnd, req.TZ); err != nil {
		a.logger.Error("quiet-hours: update", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not save quiet hours")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": 1,
		"quiet_hours": map[string]any{
			"start": req.QuietStart,
			"end":   req.QuietEnd,
			"tz":    normalizeTZ(req.TZ),
		},
	})
}

// --- normalization ----------------------------------------------------------

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func normalizeTZ(s string) string {
	if s == "" {
		return "UTC"
	}
	return s
}
