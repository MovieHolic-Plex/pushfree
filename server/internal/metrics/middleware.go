package metrics

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// RequestIDHeader is the response/request header carrying the per-request
// correlation id. The same value appears in the response header and the slog
// request log line, which is the correlation contract from the todo-15 SPEC.
const RequestIDHeader = "X-Request-ID"

// wsPath is the WebSocket route tracked by the pushfree_ws_clients gauge.
// Tracked here (rather than inside the hub) so the metrics package owns the
// full observation surface and does not require the hub to be wired to a
// *Metrics value.
const wsPath = "/1/ws"

// sendPath is the Pushover-compatible send route whose 2xx responses count
// as one accepted send == one received message.
const sendPath = "/1/messages.json"

// statusWriter captures the response status code so the middleware can log
// it and derive send/receive observations from it. It delegates every
// ResponseWriter method to the embedded writer. Write defaults status to 200
// to match net/http's behavior when a handler writes a body without calling
// WriteHeader.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap exposes the underlying http.ResponseWriter so capability-sensitive
// callers can reach interfaces statusWriter does not implement itself:
// http.Hijacker (required by the coder/websocket upgrade at GET /1/ws) and
// http.Flusher (required by the SSE streaming handler). Without it,
// websocket.Accept finds neither Hijacker nor an Unwrapper on statusWriter
// and returns HTTP 501, breaking every live WS connection. The metrics
// wrapper still observes WriteHeader/Write; a hijacked WS writes its upgrade
// response through the raw conn, which statusOrZero reports honestly as 0.
// Unwrap exposes the underlying http.ResponseWriter so capability-sensitive
// callers can reach interfaces statusWriter does not implement itself:
// http.Hijacker (required by the coder/websocket upgrade at GET /1/ws) and
// http.Flusher (required by the SSE streaming handler). Without it,
// websocket.Accept finds neither Hijacker nor an Unwrapper on statusWriter
// and returns HTTP 501, breaking every live WS connection. The metrics
// wrapper still observes WriteHeader/Write; a hijacked WS writes its upgrade
// response through the raw conn, which statusOrZero reports honestly as 0.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RequestLogger returns middleware that assigns each request a stable
// request_id, echoes it in the X-Request-ID response header, and emits one
// slog line per request carrying request_id, method, path, status, and
// duration_ms. It also records pushfree_* observations derived from the
// request: sends/messages_received on a 2xx POST /1/messages.json, and the
// live WebSocket client gauge for GET /1/ws (incremented on enter,
// decremented when the handler returns, i.e. when the connection closes).
//
// If the inbound request already carries an X-Request-ID it is reused;
// otherwise a fresh UUIDv4 is generated. In both cases the response header
// and the log line carry the identical value.
func RequestLogger(logger *slog.Logger, m *Metrics) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(RequestIDHeader)
			if requestID == "" {
				requestID = uuid.NewString()
			}
			w.Header().Set(RequestIDHeader, requestID)

			sw := &statusWriter{ResponseWriter: w}

			trackWS := r.Method == http.MethodGet && r.URL.Path == wsPath
			if trackWS {
				m.IncWSClients()
				defer m.DecWSClients()
			}

			start := time.Now()
			next.ServeHTTP(sw, r)
			durationMs := time.Since(start).Milliseconds()

			// A 2xx send is exactly one accepted send == one received
			// message. Non-2xx (validation/auth/quota) does not count.
			if r.Method == http.MethodPost && r.URL.Path == sendPath && sw.status >= 200 && sw.status < 300 {
				m.IncSends()
				m.IncMessagesReceived()
			}

			logger.Info("request",
				"request_id", requestID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", statusOrZero(sw.status),
				"duration_ms", durationMs,
			)
		})
	}
}

// statusOrZero reports the captured status, or 0 if the handler wrote nothing
// at all (e.g. a hijacked WebSocket connection whose upgrade response was
// written through the underlying conn rather than WriteHeader). 0 is the
// honest "no HTTP response status observed" value for the log line.
func statusOrZero(s int) int { return s }
