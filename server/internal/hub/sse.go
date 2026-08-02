package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ServeSSE handles GET /1/sse?secret=&device_id=&since=.
//
// It is the SSE fallback for clients that cannot use WebSocket. Auth is via
// query parameters (SSE is one-way, so there is no login frame). After auth
// it writes an "open" event, replays stored messages with id > since, then
// streams live messages as `event: message` lines, injecting a keepalive
// comment (`: keepalive`) every keepalive interval.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dev, ok := authenticateDevice(r.Context(), h.devs, q.Get("device_id"), q.Get("secret"))
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "invalid device_id or secret")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // send headers immediately

	// Subscribe BEFORE the open event so a Publish the client issues upon
	// receiving the response cannot land in a no-subscriber window.
	sub := h.subscribe(dev.UserID)
	defer h.unsubscribe(sub)

	since := parseSince(q.Get("since"))
	maxID, _ := h.store.MaxMessageID(r.Context(), dev.UserID)
	openData, _ := json.Marshal(map[string]any{"last_message_id": maxID})
	if !writeSSEEvent(w, flusher, "open", openData) {
		return
	}

	// Replay stored messages with id > since.
	lastSent := since
	msgs, err := h.store.ListMessages(r.Context(), dev.UserID, since, messagePageLimit)
	if err == nil {
		for _, m := range msgs {
			if !writeSSEEvent(w, flusher, "message", marshalMessage(m)) {
				return
			}
			lastSent = m.ID
			h.fireHook(r.Context(), m)
		}
	}

	// Live loop. r.Context() is canceled when the HTTP client disconnects
	// (SSE is a normal streaming response, not a hijacked connection).
	ctx := r.Context()
	ticker := time.NewTicker(h.keepalive)
	defer ticker.Stop()
	for {
		select {
		case m := <-sub.ch:
			if m.ID <= lastSent {
				continue // de-dup vs. the replay page.
			}
			if !writeSSEEvent(w, flusher, "message", marshalMessage(m)) {
				return
			}
			lastSent = m.ID
			h.fireHook(ctx, m)
		case <-ticker.C:
			// SSE comment frame: ignored by clients, keeps the connection alive.
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-ctx.Done():
			return
		case <-h.done:
			return
		}
	}
}

// writeSSEEvent writes one `event: <name>\ndata: <json>\n\n` frame and flushes.
// json.Marshal output contains no raw newlines (they are escaped), so the data
// field is a single line as the SSE spec requires. Returns false if the write
// failed (the connection is dead and the handler should stop).
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, name string, data []byte) bool {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
