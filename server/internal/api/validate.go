package api

import "net/http"

// validateHandler implements POST /1/users/validate.json (plan todo 11), the
// Pushover-compatible recipient-validation endpoint. Given an app token and a
// user key, it confirms the user key belongs to the token's owner and returns
// that user's registered device names plus an empty licenses array (pushfree
// is self-hosted and has no paid licenses, so licenses is always []).
//
// Auth mirrors the send path (messages.json): the app token and user key are
// read from the form body. A missing/malformed/unknown/revoked token yields
// {"status":0,"errors":["application token is invalid"]} with HTTP 400
// (Pushover returns 400 for validate.json rather than the 401 the send path
// uses). A user key that does not belong to the token's owner yields
// {"status":0,"errors":["user key is invalid"]}. On success the response is
// {"status":1,"devices":["name", ...],"licenses":[]}.
//
// This is a method on *Accounts so it shares ValidateAppToken (the app-token
// auth check) and the store Repos with the rest of the /1/* surface.
func (a *Accounts) validateHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErrors(w, http.StatusBadRequest, "could not parse form data")
		return
	}
	token := r.FormValue("token")
	userKey := r.FormValue("user")

	// --- App token (form body) ---
	// ValidateAppToken rejects empty, malformed, unknown, and revoked tokens
	// uniformly with ErrInvalidAppToken; validate.json surfaces them all as
	// the same 400 "application token is invalid" body.
	senderUserID, err := a.ValidateAppToken(r.Context(), token)
	if err != nil {
		writeErrors(w, http.StatusBadRequest, "application token is invalid")
		return
	}

	// --- Recipient must resolve to the token's owner (self) ---
	// A user key that is unknown, or that belongs to a different account, is
	// reported with the same message Pushover uses ("user key is invalid"),
	// so validate.json cannot be used to enumerate other users' devices.
	if userKey == "" {
		writeErrors(w, http.StatusBadRequest, "user key is required")
		return
	}
	u, err := a.repos.Users.GetByUserKey(r.Context(), userKey)
	if err != nil || u.ID != senderUserID {
		writeErrors(w, http.StatusBadRequest, "user key is invalid")
		return
	}

	// --- Devices: the registered names for this user, in creation order ---
	devices, err := a.repos.Devices.ListByUser(r.Context(), u.ID)
	if err != nil {
		a.logger.Error("validate.json: list devices", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not validate user")
		return
	}
	names := make([]string, 0, len(devices))
	for _, d := range devices {
		names = append(names, d.Name)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   1,
		"devices":  names,
		"licenses": []any{},
	})
}
