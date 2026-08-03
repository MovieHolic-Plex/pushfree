# Architecture

PushFree is a self-hostable, Pushover-API-compatible push notification
service. A single Go binary serves the HTTP API, the realtime WebSocket/SSE
fan-out, and an embedded admin dashboard. Native clients (Android, desktop)
speak the Open Client protocol; any existing Pushover-speaking integration
works unchanged against `POST /1/messages.json`.

This document maps the packages that implement the system. For the
per-endpoint contract and deliberate deviations from Pushover, see
[docs/API-COMPAT.md](docs/API-COMPAT.md); for runtime configuration see
[docs/configuration.md](docs/configuration.md).

## High-level data flow

```text
 sender (curl / app / monitoring)             recipient device
   |                                                ^
   | POST /1/messages.json  (app token + user key)  |  WebSocket / SSE / FCM / UnifiedPush
   v                                                |
 +--------------------- server (single Go binary) ----------------------+
 | api (net/http mux) -> store.Repos ->  sqlite  |  postgres           |
 |        |                              ^                              |
 |        | priority=2                   |                              |
 |        v                              |                              |
 | receipts (state machine) -> timers (durable retry engine)           |
 |        |                                                             |
 |        v ack                                                          |
 | callbacks (SSRF egress) ----- webhook to sender's callback_url       |
 +----------------------------------------------------------------------+
                    dashboard: web/ (Next.js static export) embed via go:embed
```

A single `POST /1/messages.json` with one `user` value may fan out to many
recipients: the `user` field accepts a comma-separated list (<= 50 keys) and a
group key, each of which expands to one stored message row per concrete
recipient. Quota is charged per concrete recipient.

## Server (`server/`)

The server is pure Go (`CGO_ENABLED=0`, the `modernc.org/sqlite` driver). It
is a single binary built from `server/cmd/pushfree`. All domain code lives
under `server/internal/`.

| Package                       | Responsibility                                                                                                  |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `cmd/pushfree`                | Entry point: loads config, wires the store, registers routes, starts the hub, sweeper, callback worker, server. |
| `internal/config`             | TOML + env config schema; startup validation (TLS all-or-nothing, version check). Source of every config key.    |
| `internal/server`             | HTTP(S) server lifecycle, graceful shutdown, WAL checkpoint tail.                                               |
| `internal/api`                | The HTTP surface: `messages.json`, `accounts`, `apps`, `groups`, `subscriptions`, `receipts`, `cancel`, `validate`, `sounds`, `quota`, `applimit`, `session`, `security`. Implements the Pushover-compatible `/1/*` and management `/v1/*` routes. |
| `internal/store`              | The `store.Repos` interface — the storage contract shared by both backends.                                      |
| `internal/store/sqlite`       | SQLite backend (default): pure-Go driver, WAL, `busy_timeout`, migrations.                                      |
| `internal/store/postgres`     | Postgres backend (`pgx/v5`): schema-parity migrations, selected behind `db-url`.                                |
| `internal/hub`                | Realtime fan-out: WebSocket (`/1/ws`), SSE (`/1/sse`), `since`-cursor replay, 45 s keepalive, device login (`POST /1/devices/login.json`, SHA-256 secret), message pull (`GET /1/messages.json?since=`). In-process, single-node. |
| `internal/quota`              | Per-user monthly quota (default 10,000), period = `YYYY-MM` in `America/Chicago`, reset at 00:00 CT.            |
| `internal/receipts`           | Emergency (priority-2) receipt lifecycle: state machine, ack, cancel, cancel-by-tag, 7-day query window, GC.    |
| `internal/timers`             | Durable retry/timer engine with crash recovery (used by the receipt retry scheduler).                            |
| `internal/callbacks`          | Best-effort receipt-JSON webhook worker: 60 s retry on non-2xx, SSRF allow-by-denylist egress policy.           |
| `internal/quiethours`         | Server-side quiet-hours hold for `priority <= 0`; `priority >= 1` bypasses.                                     |
| `internal/retention`          | Bounded retention sweeper: 30-day messages, 3-day attachment BLOBs, TTL discard; runs at startup + interval.    |
| `internal/e2ee`               | Pushover per-field E2EE format (GZIP/AES-256-CBC/HMAC/base64). Client-side reference decrypt; **server stores encrypted=1 fields opaquely and never decrypts.** |
| `internal/fcm`                | Optional FCM v1 delivery channel (env-gated by `fcm-credentials-file`).                                         |
| `internal/up`                 | UnifiedPush distributor endpoint (`/up/{sub}/*`). Not a Pushover feature; pushfree addition.                    |
| `internal/metrics`            | `GET /metrics` (Prometheus text) + structured request logging. **Not authenticated** — bind privately.          |
| `internal/webmount`           | Serves the embedded Next.js static export at `/admin/` via `go:embed`: clean-URL resolution, SPA fallback, correct MIME, immutable cache for hashed assets. |

### Storage interface and dual backend

`internal/store` defines `store.Repos`, the single storage contract. Two
backends implement it:

- `internal/store/sqlite` — default; one file (`db-file`, default
  `pushfree.db`), zero ops, good for single-node deployments up to roughly
  10,000 messages/month.
- `internal/store/postgres` — selected by setting `db-url` to a pgx connection
  string; for operators who already run Postgres or need a shared DB.

Switching backends is a config change. Migrations run automatically and
idempotently on startup (version-pinned in `schema_migrations`); there is no
automatic down-migration. See [docs/POSTGRES.md](docs/POSTGRES.md).

### Realtime fan-out and delivery channels

The hub is in-process and single-node. A device registers via
`POST /1/devices/login.json` (only `sha256(secret)` is stored) and then
connects a transport:

- **WebSocket** (`/1/ws`) — the first-class transport. Login line, `open`
  frame, replay of `since`-cursor backlog, live `message` frames, 45 s
  keepalive, close `4001` on auth failure.
- **SSE** (`/1/sse`) — fallback; a pushfree addition (Pushover has no SSE).
- **FCM v1** — optional; enabled by `fcm-credentials-file`.
- **UnifiedPush** — always available as a distributor endpoint.

Delivery is recorded as `delivered` when the transport accepts it (WS write
success / SSE flush), which updates the receipt's `last_delivered_at`. See
[docs/API-COMPAT.md](docs/API-COMPAT.md) for the deliberate deviation around
Pushover's "<= 2 concurrent TCP" rule (not enforced; documented for
self-hosters).

## Clients

### Android (`android/`)

Kotlin app under `net.pushfree.android` (Gradle, AGP, JDK 17, compileSdk 35).
Transports and key packages:

- `ws/` — WebSocket foreground service transport (the default).
- `fcm/` — optional FCM transport (`FcmPayload`, `FcmTokenRegistrar`).
- `up/` — UnifiedPush connector transport.
- `notifications/` — notification pipeline, including the Android 14
  full-screen-intent emergency channel and ack actions.
- `outbox/` — WorkManager ack outbox (offline ack drain).
- `e2ee/` — E2EE decrypt (`E2ee`, `E2eeKeyStore`) at ingest, before the
  message is stored/displayed.
- `data/` — Room database, DAOs, `since` cursor.
- `ui/` — Compose UI, onboarding, settings, subscription screens; Paparazzi
  golden screenshots in tests.

The device speaks the Open Client protocol against the server hub.

### Desktop (`desktop/`)

Rust / Tauri 2 single package (`pushfree-desktop`). Source layout:

- `src/ws.rs` — direct WebSocket client (reconnect/backoff).
- `src/notify/` — notification pipeline with dedup and ack reporting.
- `src/e2ee.rs` — E2EE decrypt module (manual AES-CBC chaining for
  byte-faithful parity with the Go reference; constant-time HMAC verify).
- `src/settings.rs` — persisted settings (incl. E2EE key), threaded through
  the WS controller.
- `src/config.rs` — runtime config.
- Tauri integration: tray icon, autostart, OS notifications.

### Web dashboard (`web/`)

Next.js 15 static export (`output: 'export'`, `basePath: '/admin'`). Built with
`pnpm build` into `web/out`, then copied into
`server/internal/webmount/web/out` and embedded into the server binary via
`go:embed`. At runtime the server serves it at `/admin/` (apps, quota, live SSE
message view, receipts, quiet-hours UI). No server-side rendering: pure static
embed + SPA shell with clean-URL resolution and SPA fallback for unknown
client routes.

## Build and release

- Server image: multi-stage `golang:1.26-alpine` ->
  `gcr.io/distroless/static-debian12:nonroot`, ~21 MB, `HEALTHCHECK` on
  `/health`. `server/Dockerfile`, `deploy/docker-compose.yml`.
- Release automation: `.goreleaser.yaml` (linux/mac/win + checksums) ships the
  server binary and the desktop/APK artifacts on tag. `.github/workflows/`.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the per-component build and test
commands, and [docs/self-hosting.md](docs/self-hosting.md) for production
operation.
