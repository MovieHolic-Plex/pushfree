package hub

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/pushfree/pushfree/internal/store"
)

// deviceLoginBody is the JSON shape accepted by POST /1/devices/login.json.
type deviceLoginBody struct {
	Name  string `json:"name"`
	OS    string `json:"os"`
	Model string `json:"model"`
}

// ServeDeviceLogin handles POST /1/devices/login.json.
//
// It resolves the authenticated user via SessionUserResolver (the wiring seam;
// todo 6 provides the real cookie-session implementation), validates the
// requested device name, then issues a device_id + secret (30 chars
// [A-Za-z0-9] from crypto/rand). Only SHA-256(secret) is stored; the plaintext
// secret is returned to the client exactly once. Response on success:
//
//	{"status":1,"device_id":"...","secret":"..."}
//
// A name longer than 25 chars or outside [A-Za-z0-9_-] yields 400 with a
// {"status":0,"errors":[...]} body.
func (h *Hub) ServeDeviceLogin(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.sessions.ResolveUserID(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	body, ok := readDeviceLoginBody(w, r)
	if !ok {
		return // readDeviceLoginBody already wrote the error.
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		// Default to a stable, valid name so clients that omit it still register.
		name = "device"
	}
	if err := validateDeviceName(name); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	deviceID, err := newSecret()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not generate device id")
		return
	}
	secret, err := newSecret()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not generate secret")
		return
	}

	dev := &store.Device{
		UserID:     userID,
		DeviceID:   deviceID,
		SecretHash: hashSecret(secret),
		Name:       name,
		Model:      body.Model,
		OS:         body.OS,
	}
	if _, err := h.devs.Create(r.Context(), dev); err != nil {
		// A UNIQUE violation on device_id is astronomically unlikely (30-char
		// crypto/rand); any store error here is a 500.
		writeAPIError(w, http.StatusInternalServerError, "could not register device")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    1,
		"device_id": deviceID,
		"secret":    secret,
	})
}

// readDeviceLoginBody extracts name/os/model from a form-encoded or JSON body.
// It writes a 400 error and returns ok=false on an unreadable body.
func readDeviceLoginBody(w http.ResponseWriter, r *http.Request) (deviceLoginBody, bool) {
	var body deviceLoginBody
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, "could not read request body")
			return deviceLoginBody{}, false
		}
		return body, true
	}
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "could not read request body")
		return deviceLoginBody{}, false
	}
	body.Name = r.FormValue("name")
	body.OS = r.FormValue("os")
	body.Model = r.FormValue("model")
	return body, true
}

// GetMessagesHandler handles GET /1/messages.json?secret=&device_id=&since=.
//
// This is the PULL endpoint (the compat POST /1/messages.json ingest is a
// different handler, owned by todo 8). It authenticates the device, then
// returns a JSON array of that user's stored messages with id > since, limited
// to 100, oldest first. since=0 or absent returns the latest page.
func (h *Hub) GetMessagesHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dev, ok := authenticateDevice(r.Context(), h.devs, q.Get("device_id"), q.Get("secret"))
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "invalid device_id or secret")
		return
	}
	since := parseSince(q.Get("since"))

	msgs, err := h.store.ListMessages(r.Context(), dev.UserID, since, messagePageLimit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not read messages")
		return
	}
	if msgs == nil {
		msgs = []StoredMessage{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// writeJSON writes a JSON body with the given status. It intentionally does
// not add a "request" id; that is the job of the request-logging middleware
// (todo 15) at wiring time.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeAPIError writes a Pushover-compatible error envelope:
//
//	{"status":0,"errors":["..."]}
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"status": 0,
		"errors": []string{msg},
	})
}

// Routes returns an *http.ServeMux with all hub routes registered. This is the
// single seam todo 8 mounts into the main server (the hub package owns no
// router state of its own). Routes:
//
//	POST /1/devices/login.json -> ServeDeviceLogin
//	GET  /1/messages.json      -> GetMessagesHandler
//	GET  /1/ws                 -> ServeWS
//	GET  /1/sse                -> ServeSSE
func (h *Hub) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /1/devices/login.json", h.ServeDeviceLogin)
	mux.HandleFunc("GET /1/messages.json", h.GetMessagesHandler)
	mux.HandleFunc("GET /1/ws", h.ServeWS)
	mux.HandleFunc("GET /1/sse", h.ServeSSE)
	return mux
}
