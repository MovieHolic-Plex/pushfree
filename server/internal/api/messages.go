package api

import (
	"encoding/base64"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/pushfree/pushfree/internal/store"
)

// Pushover-compatible send field limits. message is measured in UTF-8 RUNES
// (utf8.RuneCountInString), not bytes; the others are rune counts too, which
// matches the plan's boundary tests (ASCII payloads, where runes == bytes).
const (
	maxMessageRunes   = 1024
	maxTitleRunes     = 250
	maxURLRunes       = 512
	maxURLTitleRunes  = 100
	maxDeviceLen      = 25
	maxAttachmentSize = 5_242_880 // 5 MiB, Pushover's hard attachment cap
	// maxRequestSize bounds the whole request body so an oversized upload is
	// aborted early by MaxBytesReader. It leaves headroom over the attachment
	// cap for the base64 encoding of a max-size attachment (~4/3x) plus the
	// form fields.
	maxRequestSize = maxAttachmentSize + 4<<20
)

// deviceTokenRe matches a single device name: [A-Za-z0-9_-]+ (ASCII, so byte
// length == rune count). The "device" field is a comma-separated list of these.
var deviceTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// messagesHandler implements POST /1/messages.json, the full Pushover-compatible
// send contract (plan todo 8). It is a method on *Accounts so it shares the
// app-token validation helper (ValidateAppToken) and the X-Limit-App-* header
// helpers (MarkSendAccepted / SetLimitHeaders) with the rest of the /1/* surface.
//
// Auth: the app token is read from the form body. A missing token is a 400
// validation error (collected with the other field errors); a present-but-
// invalid/revoked token is 401. The "user" field must resolve to the token's
// owner for todo 8 (single-recipient / self); multi-user fan-out is todo 9.
//
// Every response carries a per-request UUID in the "request" field. On
// success: {"status":1,"request":"<uuid>"} and, for priority=2, an additional
// 30-char [A-Za-z0-9] "receipt". On error: {"status":0,"errors":[...],"request":"<uuid>"}.
func (a *Accounts) messagesHandler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.NewString()

	// Bound the body so an oversized upload is rejected at the read boundary.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestSize)

	// Parse form: multipart (carries a file attachment) or urlencoded. A parse
	// failure (including a MaxBytesReader trip) is a 400.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(maxAttachmentSize); err != nil {
			writeRequestErrors(w, http.StatusBadRequest, requestID, "could not parse multipart request")
			return
		}
	} else if err := r.ParseForm(); err != nil {
		writeRequestErrors(w, http.StatusBadRequest, requestID, "could not parse form data")
		return
	}

	token := r.FormValue("token")
	userKey := r.FormValue("user")
	message := r.FormValue("message")

	// --- Auth ---------------------------------------------------------------
	// A present-but-invalid token short-circuits to 401 (per plan). A missing
	// token is collected below as a 400 field error alongside user/message.
	var senderUserID, appID int64
	if token != "" {
		uid, err := a.ValidateAppToken(r.Context(), token)
		if err != nil {
			writeRequestErrors(w, http.StatusUnauthorized, requestID, "application token is invalid")
			return
		}
		app, err := a.repos.Apps.GetByToken(r.Context(), token)
		if err != nil {
			// ValidateAppToken resolved it moments ago; a vanishing row is
			// treated as an invalid token rather than a 500.
			writeRequestErrors(w, http.StatusUnauthorized, requestID, "application token is invalid")
			return
		}
		senderUserID = uid
		appID = app.ID
	}

	var errs []string
	if token == "" {
		errs = append(errs, "token is required")
	}

	// --- Recipient (single-recipient resolution; todo 9 expands to fan-out) -
	// Only resolvable once the caller is authenticated. The recipient must be
	// the token's owner for todo 8 (self-send); a different/unknown user_key
	// is reported with the same message Pushover uses for an invalid key.
	var recipientUserID int64
	if token != "" {
		if userKey == "" {
			errs = append(errs, "user key is required")
		} else {
			u, err := a.repos.Users.GetByUserKey(r.Context(), userKey)
			if err != nil || u.ID != senderUserID {
				errs = append(errs, "user key is invalid")
			} else {
				recipientUserID = u.ID
			}
		}
	} else if userKey == "" {
		errs = append(errs, "user key is required")
	}

	if message == "" {
		errs = append(errs, "message is required")
	}

	// --- Field validation ---------------------------------------------------
	if message != "" && utf8.RuneCountInString(message) > maxMessageRunes {
		errs = append(errs, "message must be 1024 characters or fewer")
	}
	title := r.FormValue("title")
	if utf8.RuneCountInString(title) > maxTitleRunes {
		errs = append(errs, "title must be 250 characters or fewer")
	}
	urlVal := r.FormValue("url")
	if utf8.RuneCountInString(urlVal) > maxURLRunes {
		errs = append(errs, "url must be 512 characters or fewer")
	}
	urlTitle := r.FormValue("url_title")
	if utf8.RuneCountInString(urlTitle) > maxURLTitleRunes {
		errs = append(errs, "url_title must be 100 characters or fewer")
	}

	priority := 0
	if pv := r.FormValue("priority"); pv != "" {
		n, err := strconv.Atoi(pv)
		if err != nil || n < -2 || n > 2 {
			errs = append(errs, "priority must be an integer between -2 and 2")
		} else {
			priority = n
		}
	}

	device := r.FormValue("device")
	if device != "" {
		for _, d := range strings.Split(device, ",") {
			d = strings.TrimSpace(d)
			if d == "" || len(d) > maxDeviceLen || !deviceTokenRe.MatchString(d) {
				errs = append(errs, "device must be a comma-separated list of names up to 25 chars [A-Za-z0-9_-]")
				break
			}
		}
	}

	htmlOn := formBool(r.FormValue("html"))
	monoOn := formBool(r.FormValue("monospace"))
	if htmlOn && monoOn {
		errs = append(errs, "html and monospace are mutually exclusive")
	}

	var ttl int64
	if tv := r.FormValue("ttl"); tv != "" {
		n, err := strconv.ParseInt(tv, 10, 64)
		if err != nil || n < 0 {
			errs = append(errs, "ttl must be a non-negative number of seconds")
		} else {
			ttl = n
		}
	}

	// timestamp is accepted as-is (Pushover lets the caller back/fore-date).
	// An unparseable value is ignored rather than rejected.
	var timestamp int64
	if tv := r.FormValue("timestamp"); tv != "" {
		if n, err := strconv.ParseInt(tv, 10, 64); err == nil {
			timestamp = n
		}
	}

	// --- Attachment (single, <= maxAttachmentSize) --------------------------
	// Either a multipart "attachment" file OR attachment_base64+attachment_type.
	// Over the limit or undecodable -> collected as a 400 error.
	var attachment *store.Attachment
	if file, header, err := r.FormFile("attachment"); err == nil {
		if header.Size > maxAttachmentSize {
			errs = append(errs, "attachment must be 5242880 bytes or fewer")
		} else {
			data, rerr := io.ReadAll(file)
			if rerr != nil {
				errs = append(errs, "could not read attachment")
			} else if len(data) > maxAttachmentSize {
				errs = append(errs, "attachment must be 5242880 bytes or fewer")
			} else {
				attachment = &store.Attachment{ContentType: header.Header.Get("Content-Type"), Data: data}
			}
		}
		_ = file.Close()
	} else if attachment == nil && r.FormValue("attachment_base64") != "" {
		data, derr := base64.StdEncoding.DecodeString(r.FormValue("attachment_base64"))
		if derr != nil {
			errs = append(errs, "attachment_base64 is not valid base64")
		} else if len(data) > maxAttachmentSize {
			errs = append(errs, "attachment must be 5242880 bytes or fewer")
		} else {
			attachment = &store.Attachment{ContentType: r.FormValue("attachment_type"), Data: data}
		}
	}

	if len(errs) > 0 {
		writeRequestErrors(w, http.StatusBadRequest, requestID, errs...)
		return
	}

	// --- Assemble the send row ---------------------------------------------
	// sound: unknown/custom values are ACCEPT-AND-STORE. Pushover falls back
	// to its default sound for user-uploaded/unknown sounds, so we do NOT
	// reject them here -- we record whatever the caller sent.
	sound := r.FormValue("sound")
	tags := r.FormValue("tags")
	callback := r.FormValue("callback")

	now := time.Now().UTC()
	send := store.Send{
		AppID:        appID,
		SenderUserID: senderUserID,
		Priority:     priority,
		Sound:        sound,
		Title:        title,
		Body:         message,
		URL:          urlVal,
		URLTitle:     urlTitle,
		HTML:         htmlOn,
		Monospace:    monoOn,
		Timestamp:    timestamp,
		TTL:          ttl,
		Tag:          tags,
		Encrypted:    formBool(r.FormValue("encrypted")),
		CallbackURL:  callback,
		CreatedAt:    now,
	}
	msg := store.Message{
		RecipientUserID: recipientUserID,
		DeviceFilter:    device,
		CreatedAt:       now,
	}

	// priority=2: create a pending receipt placeholder (30-char id) matching
	// the receipts schema. The retry/expire lifecycle is wired by todos 20-21;
	// here we only seed the row in its initial state.
	var receipt *store.Receipt
	if priority == 2 {
		rid, err := newUserKey() // 30-char [A-Za-z0-9], same generator as user_key/token
		if err != nil {
			a.logger.Error("messages.json: generate receipt id", "err", err)
			writeRequestErrors(w, http.StatusInternalServerError, requestID, "could not send message")
			return
		}
		receipt = &store.Receipt{ID: rid, State: "pending", Tag: tags}
		send.ReceiptID = rid
	}

	if _, err := a.repos.Ingests.Ingest(r.Context(), &store.IngestInput{
		Send:       send,
		Messages:   []store.Message{msg},
		Receipt:    receipt,
		Attachment: attachment,
	}); err != nil {
		a.logger.Error("messages.json: ingest", "err", err)
		writeRequestErrors(w, http.StatusInternalServerError, requestID, "could not send message")
		return
	}

	// --- Quota: consume on accepted send, then attach the live headers -----
	// MarkSendAccepted does not enforce the limit (todo 10 adds the 429 gate);
	// a failure to record is logged but does not fail an otherwise-accepted
	// send. SetLimitHeaders overwrites the placeholder headers limitWrap wrote
	// from the (body-resolved) caller, so X-Limit-App-Remaining reflects this
	// send.
	if _, err := a.MarkSendAccepted(r.Context(), senderUserID); err != nil {
		a.logger.Error("messages.json: mark send accepted", "err", err)
	}
	a.SetLimitHeaders(w, senderUserID)

	resp := map[string]any{"status": 1, "request": requestID}
	if priority == 2 {
		resp["receipt"] = receipt.ID
	}
	writeJSON(w, http.StatusOK, resp)
}

// formBool maps the Pushover flag convention: only the literal "1" is true.
// Every other value (including "0", "true", "") is false, matching how
// Pushover treats html=1 / monospace=1 / encrypted=1.
func formBool(v string) bool { return v == "1" }

// writeRequestErrors writes a Pushover-style error envelope carrying the
// per-request id that /1/messages.json includes on every response:
// {"status":0,"errors":[...],"request":"<uuid>"}. It complements writeErrors
// (used by /v1/* routes, which carry no request id).
func writeRequestErrors(w http.ResponseWriter, status int, request string, errs ...string) {
	writeJSON(w, status, map[string]any{"status": 0, "errors": errs, "request": request})
}
