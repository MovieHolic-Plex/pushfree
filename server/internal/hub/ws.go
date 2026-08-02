package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// wsLoginFrame is the first line a client sends after the WS upgrade.
type wsLoginFrame struct {
	Type     string `json:"type"`
	DeviceID string `json:"device_id"`
	Secret   string `json:"secret"`
}

// ServeWS handles GET /1/ws?since=.
//
// Upgrade -> read one login line -> validate -> reply open -> replay stored
// messages with id > since -> stream live messages + keepalives. Any auth
// failure (unknown device, wrong secret, missing/malformed login) closes the
// connection with application code 4001.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	// InsecureSkipVerify: Open Client connections are native apps (Android,
	// desktop), not browsers, so the Origin header carries no trust signal.
	// Origin allow-listing is a wiring-time concern (todo 8) recorded in
	// docs/api-compat.md.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// 1. Login frame, bounded by the read timeout.
	loginCtx, cancel := context.WithTimeout(r.Context(), h.readTtl)
	_, line, err := conn.Read(loginCtx)
	cancel()
	if err != nil {
		_ = conn.Close(wsCloseAuth, "login required")
		return
	}
	var login wsLoginFrame
	if err := json.Unmarshal(line, &login); err != nil || login.Type != "login" {
		_ = conn.Close(wsCloseAuth, "bad login frame")
		return
	}
	dev, ok := authenticateDevice(r.Context(), h.devs, login.DeviceID, login.Secret)
	if !ok {
		_ = conn.Close(wsCloseAuth, "invalid device_id or secret")
		return
	}

	// Subscribe BEFORE sending any client-observable response. The client can
	// react to the open frame by publishing; if subscription happened after
	// open, a Publish in that window would find no subscribers and the message
	// would be silently dropped. Subscribe-before-open closes that race.
	sub := h.subscribe(dev.UserID)
	defer h.unsubscribe(sub)

	// Open frame with the server's high-water mark.
	since := parseSince(r.URL.Query().Get("since"))
	maxID, _ := h.store.MaxMessageID(r.Context(), dev.UserID)
	if err := writeWSJSON(r.Context(), conn, map[string]any{
		"type":            "open",
		"last_message_id": maxID,
	}); err != nil {
		return
	}

	// Replay stored messages with id > since (oldest first).
	lastSent := since
	msgs, err := h.store.ListMessages(r.Context(), dev.UserID, since, messagePageLimit)
	if err == nil {
		for _, m := range msgs {
			if err := writeWSRaw(r.Context(), conn, marshalMessage(m)); err != nil {
				return
			}
			lastSent = m.ID
			h.fireHook(r.Context(), m)
		}
	}

	// 5. Live loop. CloseRead returns a context canceled when the client
	//    disconnects (its reader goroutine ends), which is how we detect a
	//    gone-but-silent client in addition to write failures.
	ctx := conn.CloseRead(r.Context())
	ticker := time.NewTicker(h.keepalive)
	defer ticker.Stop()
	for {
		select {
		case m := <-sub.ch:
			if m.ID <= lastSent {
				continue // de-dup vs. the replay page (subscribe-before-replay overlap).
			}
			if err := writeWSRaw(ctx, conn, marshalMessage(m)); err != nil {
				return
			}
			lastSent = m.ID
			h.fireHook(ctx, m)
		case <-ticker.C:
			if err := writeWSRaw(ctx, conn, keepaliveFrame); err != nil {
				return
			}
		case <-ctx.Done():
			return
		case <-h.done:
			return
		}
	}
}

// writeWSRaw writes a single pre-rendered text frame.
func writeWSRaw(ctx context.Context, conn *websocket.Conn, frame []byte) error {
	return conn.Write(ctx, websocket.MessageText, frame)
}

// writeWSJSON marshals v and writes it as a text frame.
func writeWSJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeWSRaw(ctx, conn, b)
}
