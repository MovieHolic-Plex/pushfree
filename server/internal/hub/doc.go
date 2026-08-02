// Package hub implements pushfree's in-process real-time delivery fan-out:
// the Open Client transports (WebSocket and SSE), the device registration and
// message-pull HTTP handlers, and the since-replay protocol.
//
// # Scope and wiring seam
//
// This package is deliberately self-contained: it owns the /1/devices/login.json,
// /1/messages.json (pull), /1/ws and /1/sse routes but does NOT mount them into
// the main server. Mounting is deferred to todo 8 (server wiring) because the
// session middleware (todo 6) that backs SessionUserResolver does not exist yet.
// Tests exercise the routes through httptest. The package exposes Hub.Routes so
// wiring is a one-liner once the session resolver exists.
//
// # Protocol (Open Client)
//
//	POST /1/devices/login.json   register a device, get device_id + secret
//	GET  /1/messages.json        pull stored messages (?secret=&device_id=&since=)
//	GET  /1/ws                   WebSocket (?since=); first line is a JSON login
//	GET  /1/sse                  SSE fallback (?secret=&device_id=&since=)
//
// Authentication for the device-scoped endpoints is SHA-256(secret) matched
// against devices.secret_hash (the schema stores only the hash). The WS login
// frame is {"type":"login","device_id":"...","secret":"..."} terminated by a
// newline; on success the server replies {"type":"open","last_message_id":N},
// replays every stored message with id > since as a {"type":"message",...} line,
// then streams live events. A {"type":"keepalive"} line is injected every
// keepalive interval (45s in production, injectable for tests).
//
// # Constants (from research W2-ws.md)
//
//   - keepalive interval: 45s (sits under nginx's 60s idle timeout)
//   - read timeout: 77s (applied to the WS login frame; post-login liveness is
//     detected via keepalive write failure and conn.CloseRead, NOT a hard read
//     deadline, because Open Client connections are otherwise silent after login)
//
// # Deviations from Pushover (see docs/api-compat.md)
//
//   - The "<=2 concurrent TCP" limit is NOT enforced (self-hosted; recorded).
//   - WebSocket library: github.com/coder/websocket (maintained nhooyr fork,
//     pure Go so the single static binary / CGO_ENABLED=0 build is preserved).
package hub
