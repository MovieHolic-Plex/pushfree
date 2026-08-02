// Package up implements pushfree's UnifiedPush distributor endpoints.
//
// These routes expose the Open Client device model under ntfy-parity URLs so a
// UnifiedPush connector can register an endpoint, poll messages, and
// acknowledge receipt without speaking the Pushover wire format:
//
//	POST /up/{sub}/subscribe.json  create a device registration -> device_id + secret
//	GET  /up/{sub}/messages.json   pull stored messages (?device_id=&secret=&since=)
//	POST /up/{sub}/ack/{msg}       idempotent no-op 200 (ntfy-parity)
//
// The subscription key {sub} is a 4-char [A-Za-z0-9] string derived
// deterministically from the device_id (HMAC-SHA256 keyed by the server's
// auth secret). Derivation is one-way, so messages.json/ack authenticate the
// device with its device_id+secret (reusing the hub's SHA-256(secret) scheme)
// and additionally verify that DeriveSub(device_id) matches the path {sub}.
//
// Naming note: this is the UnifiedPush distributor surface, NOT the
// user-to-user subscription codes of todo 12 (/1/subscriptions). The two share
// the word "subscription" but are unrelated endpoints.
//
// The device-registration helpers (newSecret, hashSecret, validateDeviceName,
// authenticateDevice, parseSince) mirror internal/hub/auth.go. They are
// reimplemented here rather than imported because the hub package keeps them
// unexported and this package must not edit hub (owned by todo 13). The logic
// is frozen by the schema and the Open Client protocol.
package up

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pushfree/pushfree/internal/store"
)

// Identifier and subscription constants.
const (
	// idLength is the length of device_id and secret values: 30 chars
	// [A-Za-z0-9], matching the pushfree identifier format and the hub's
	// device registration.
	idLength = 30
	// idAlphabet is the [A-Za-z0-9] space from which identifiers are drawn.
	idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	// subAlphabet is the [A-Za-z0-9] space for the 4-char subscription key.
	subAlphabet = idAlphabet
	// subLength is the length of the derived subscription key.
	subLength = 4
	// messagePageLimit caps a messages pull page, oldest first (mirrors hub).
	messagePageLimit = 100
	// defaultDeviceName is used when the subscribe request omits a name, so a
	// connector that sends no name still registers a valid device.
	defaultDeviceName = "up"
)

var (
	// subRe matches the 4-char [A-Za-z0-9] subscription key.
	subRe = regexp.MustCompile(`^[A-Za-z0-9]{4}$`)
	// deviceNameRe is the allowed device-name character class (mirrors hub).
	deviceNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// SessionUserResolver resolves the authenticated account from a session cookie
// for the subscribe endpoint (which binds a new device to a user). The
// production implementation is api.SessionResolver; tests inject a stub. The
// method signature matches hub.SessionUserResolver so a single resolver
// satisfies both.
type SessionUserResolver interface {
	ResolveUserID(r *http.Request) (userID int64, ok bool)
}

// Options configures a Handler. Zero-value fields take documented defaults.
type Options struct {
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Handler is the UnifiedPush distributor HTTP surface. It holds only the
// narrow repo slices it needs (Devices, Messages, Sends).
type Handler struct {
	devs     store.DeviceRepo
	msgs     store.MessageRepo
	sends    store.SendRepo
	sessions SessionUserResolver
	hmacKey  []byte
	log      *slog.Logger
}

// New builds a UP handler. authSecret keys the deterministic subscription-key
// derivation (DeriveSub); sessions resolves the subscribing user (subscribe is
// session-authenticated, like /1/devices/login.json).
func New(repos store.Repos, sessions SessionUserResolver, authSecret []byte, opts Options) *Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Handler{
		devs:     repos.Devices,
		msgs:     repos.Messages,
		sends:    repos.Sends,
		sessions: sessions,
		hmacKey:  authSecret,
		log:      opts.Logger,
	}
}

// Register mounts the UnifiedPush routes on mux:
//
//	POST /up/{sub}/subscribe.json
//	GET  /up/{sub}/messages.json
//	POST /up/{sub}/ack/{msg}
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /up/{sub}/subscribe.json", h.ServeSubscribe)
	mux.HandleFunc("GET /up/{sub}/messages.json", h.ServeMessages)
	mux.HandleFunc("POST /up/{sub}/ack/{msg}", h.ServeAck)
}

// --- device-registration helpers (mirror internal/hub/auth.go) -------------

// newSecret generates a cryptographically random idLength-char identifier. It
// uses crypto/rand.Int (math/big) per character so the distribution is
// unbiased (modulo on 62 would skew toward the first 8 symbols).
func newSecret() (string, error) {
	out := make([]byte, idLength)
	max := big.NewInt(int64(len(idAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = idAlphabet[n.Int64()]
	}
	return string(out), nil
}

// hashSecret returns the lower-case hex SHA-256 of secret, the form stored in
// devices.secret_hash. Only the hash is persisted; the plaintext is returned
// to the client exactly once at registration.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// validateDeviceName enforces name <= 25 chars (UTF-8 rune count) over the
// [A-Za-z0-9_-] class, matching the schema CHECK and the hub's check.
func validateDeviceName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if utf8.RuneCountInString(name) > 25 {
		return errors.New("name must be 25 characters or fewer")
	}
	if !deviceNameRe.MatchString(name) {
		return errors.New("name may only contain letters, digits, underscore and hyphen")
	}
	return nil
}

// parseSince parses the since cursor (a message id). An absent/empty/negative
// value yields 0 (the latest page). It never returns an error: a malformed
// since is treated as 0 so a bad query cannot 500 the endpoint.
func parseSince(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// authenticateDevice validates a (device_id, secret) pair against the stored
// hash. An unknown device, empty credential, or hash mismatch all yield
// ok=false; callers map that to a single auth-failure outcome without
// distinguishing the cause, so it cannot be used to enumerate devices.
func authenticateDevice(ctx context.Context, devs store.DeviceRepo, deviceID, secret string) (store.Device, bool) {
	if deviceID == "" || secret == "" {
		return store.Device{}, false
	}
	dev, err := devs.GetByDeviceID(ctx, deviceID)
	if err != nil {
		return store.Device{}, false
	}
	if hashSecret(secret) != dev.SecretHash {
		return store.Device{}, false
	}
	return dev, true
}

// --- subscription-key derivation -------------------------------------------

// DeriveSub returns the 4-char [A-Za-z0-9] subscription key derived
// deterministically from deviceID via HMAC-SHA256 keyed by the server's auth
// secret. The mapping is stable: the same (key, deviceID) always yields the
// same sub, so a client can recompute it after register. Rejection sampling
// over the HMAC bytes avoids the modulo bias a raw %62 would introduce (the
// same approach the account package uses for user_key). Tests MUST compute the
// expected sub via DeriveSub rather than hardcoding the HMAC output.
func DeriveSub(key []byte, deviceID string) string {
	sum := hmacSum(key, deviceID)
	const slots = len(subAlphabet)      // 62
	const limit = (256 / slots) * slots // 248: keep only unbiased byte values
	out := make([]byte, 0, subLength)
	for len(out) < subLength {
		for _, b := range sum {
			if int(b) < limit {
				out = append(out, subAlphabet[int(b)%slots])
				if len(out) == subLength {
					break
				}
			}
		}
		if len(out) < subLength {
			// Astronomically unlikely (32 bytes feed 4 chars), but stay
			// deterministic and total rather than failing: re-hash the digest.
			sum = hmacSum(key, string(sum))
		}
	}
	return string(out)
}

// hmacSum returns the SHA-256 HMAC of data under key.
func hmacSum(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// --- message shape ---------------------------------------------------------

// Message is one resolved fan-out row ready for UnifiedPush delivery. It joins
// a per-recipient message row (id) with its parent send's content; the field
// set and JSON keys mirror the hub's StoredMessage so a connector consuming
// either transport sees the same payload.
type Message struct {
	ID        int64  `json:"id"`
	SendID    int64  `json:"send_id"`
	Priority  int    `json:"priority"`
	Sound     string `json:"sound,omitempty"`
	Title     string `json:"title,omitempty"`
	Body      string `json:"message"`
	URL       string `json:"url,omitempty"`
	URLTitle  string `json:"url_title,omitempty"`
	HTML      bool   `json:"html,omitempty"`
	Monospace bool   `json:"monospace,omitempty"`
	Timestamp int64  `json:"timestamp"`
	TTL       int64  `json:"ttl,omitempty"`
	Tag       string `json:"tag,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

// fromRow joins a stored message row with its parent send into a Message.
func fromRow(m store.Message, s store.Send) Message {
	return Message{
		ID:        m.ID,
		SendID:    m.SendID,
		Priority:  s.Priority,
		Sound:     s.Sound,
		Title:     s.Title,
		Body:      s.Body,
		URL:       s.URL,
		URLTitle:  s.URLTitle,
		HTML:      s.HTML,
		Monospace: s.Monospace,
		Timestamp: s.Timestamp,
		TTL:       s.TTL,
		Tag:       s.Tag,
		Encrypted: s.Encrypted,
	}
}

// --- handlers --------------------------------------------------------------

// subscribeBody is the JSON/form shape accepted by POST subscribe.json.
type subscribeBody struct {
	Name  string `json:"name"`
	OS    string `json:"os"`
	Model string `json:"model"`
}

// ServeSubscribe handles POST /up/{sub}/subscribe.json.
//
// It requires an authenticated session (the device is bound to that user, same
// as /1/devices/login.json), validates the requested device name, then issues a
// device_id + secret (30 chars [A-Za-z0-9]). Only SHA-256(secret) is stored.
// The deterministic subscription key for the new device is derived and returned
// so the connector knows which {sub} to use for messages.json/ack. The path
// {sub} is validated for format (4-char [A-Za-z0-9]) but is not the
// authoritative key: it cannot be, because device_id is created in this call
// and the sub is derived from it. Response on success:
//
//	{"status":1,"device_id":"...","secret":"...","sub":"XXXX"}
func (h *Handler) ServeSubscribe(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.sessions.ResolveUserID(r)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !subRe.MatchString(r.PathValue("sub")) {
		writeAPIError(w, http.StatusBadRequest, "subscription key must be 4 chars [A-Za-z0-9]")
		return
	}

	body, ok := readSubscribeBody(w, r)
	if !ok {
		return // readSubscribeBody already wrote the error.
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = defaultDeviceName
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
		writeAPIError(w, http.StatusInternalServerError, "could not register device")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    1,
		"device_id": deviceID,
		"secret":    secret,
		"sub":       DeriveSub(h.hmacKey, deviceID),
	})
}

// readSubscribeBody extracts name/os/model from a JSON or form-encoded body.
func readSubscribeBody(w http.ResponseWriter, r *http.Request) (subscribeBody, bool) {
	var body subscribeBody
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeAPIError(w, http.StatusBadRequest, "could not read request body")
			return subscribeBody{}, false
		}
		return body, true
	}
	if err := r.ParseForm(); err != nil {
		writeAPIError(w, http.StatusBadRequest, "could not read request body")
		return subscribeBody{}, false
	}
	body.Name = r.FormValue("name")
	body.OS = r.FormValue("os")
	body.Model = r.FormValue("model")
	return body, true
}

// ServeMessages handles GET /up/{sub}/messages.json?device_id=&secret=&since=.
//
// It authenticates the device, verifies the path {sub} matches the device's
// derived subscription key, then returns a JSON array of that user's stored
// messages with id > since, limited to 100, oldest first. An empty result is
// the JSON array "[]" (never null).
func (h *Handler) ServeMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dev, ok := authenticateDevice(r.Context(), h.devs, q.Get("device_id"), q.Get("secret"))
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "invalid device_id or secret")
		return
	}
	if DeriveSub(h.hmacKey, dev.DeviceID) != r.PathValue("sub") {
		writeAPIError(w, http.StatusNotFound, "no such subscription")
		return
	}

	since := parseSince(q.Get("since"))
	rows, err := h.msgs.ListSince(r.Context(), dev.UserID, since, messagePageLimit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not read messages")
		return
	}
	out := make([]Message, 0, len(rows))
	for _, m := range rows {
		sd, err := h.sends.GetByID(r.Context(), m.SendID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "could not read messages")
			return
		}
		out = append(out, fromRow(m, sd))
	}
	writeJSON(w, http.StatusOK, out)
}

// ServeAck handles POST /up/{sub}/ack/{msg}.
//
// It is an idempotent no-op that always returns 200 {"status":1} (ntfy-parity:
// the UnifiedPush app acknowledgement is recorded by the endpoint, and the
// pushfree hub already marks delivery on transport accept, so there is no
// additional state to mutate here). Repeated acks for the same or any message
// are all 200.
func (h *Handler) ServeAck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": 1})
}

// --- JSON helpers ----------------------------------------------------------

// writeJSON writes a JSON body with the given status.
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
