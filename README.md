# PushFree

PushFree is a self-hostable, [Pushover](https://pushover.net)-API-compatible
push notification service. It is open source (Apache-2.0): you run the server,
you own the data, you pay nothing per message.

A single small Go binary serves the API, the realtime WebSocket/SSE fan-out,
and the embedded admin dashboard. Native clients (Android, desktop) speak the
Open Client protocol; anything that already speaks Pushover's `messages.json`
works unchanged.

## Badges

<!-- badges: CI status, license, latest version -->

## Quickstart

You need an installed Go toolchain (1.26+) to build from source.

```sh
# 1. Build the single static binary (pure Go, no cgo).
cd server
go build -o pushfree ./cmd/pushfree

# 2. Create an account. The FIRST account becomes the admin.
./pushfree &  # listens on :2586 by default
curl -sf -X POST http://localhost:2586/v1/accounts \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse"}'

# 3. Log in (sets a session cookie) and create an app token.
curl -sc cookies.txt -X POST http://localhost:2586/v1/accounts/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse"}'
TOKEN=$(curl -sb cookies.txt -X POST http://localhost:2586/v1/apps \
  -H 'Content-Type: application/json' -d '{"name":"monitoring"}' | sed -E 's/.*"token":"([^"]+)".*/\1/')
USERKEY=$(curl -sb cookies.txt http://localhost:2586/v1/accounts/me | sed -E 's/.*"user_key":"([^"]+)".*/\1/')

# 4. Send a message (Pushover-compatible).
curl -sf -X POST http://localhost:2586/1/messages.json \
  -d "token=$TOKEN" -d "user=$USERKEY" -d "message=hello from pushfree"

# 5. Health check.
curl -sf http://localhost:2586/health   # -> {"status":"ok"}
```

### Docker

A multi-stage distroless image and a `deploy/docker-compose.yml` (server, with
an optional Postgres service via the `postgres` profile) are provided. The
image build lives at `server/Dockerfile` and the compose file at
`deploy/docker-compose.yml`. Build and run:

```sh
docker build -t pushfree server/
docker compose -f deploy/docker-compose.yml up -d
curl -sf localhost:2586/health   # -> {"status":"ok"}
```

See [docs/self-hosting.md](docs/self-hosting.md) for TLS, reverse-proxy,
backup, and Postgres options.

## Features

What is actually implemented (each maps to a completed plan todo; see
[docs/](docs/) for the contracts):

- **Pushover-compatible send API** — `POST /1/messages.json` with the full
  field contract (message/title/url limits in UTF-8 runes, priority `-2..2`,
  `html`/`monospace` mutual exclusion, single attachment <= 5 MiB, `ttl`,
  `tags`, `callback`, `encrypted`). `server/internal/api/messages.go`
- **Accounts & sessions** — open signup, first account is admin, argon2id
  passwords (RFC 9106), HMAC-signed session cookies, quiet-hours settings.
  `server/internal/api/accounts.go`, `server/internal/api/security.go`
- **App tokens & rate-limit headers** — `POST/GET/DELETE /v1/apps`; every
  `/1/*` response carries `X-Limit-App-Limit/Remaining/Reset`.
  `server/internal/api/apps.go`, `server/internal/api/applimit.go`
- **Monthly quota** — 10,000 sends/user/month, reset at 00:00 America/Chicago,
  `GET /1/apps/limits.json`, pre-write 429 gate. `server/internal/api/quota.go`,
  `server/internal/quota/quota.go`
- **Multi-user fan-out & groups** — comma-separated `user` list (<= 50 keys),
  delivery groups (CRUD), one quota unit per concrete recipient.
  `server/internal/api/groups.go`
- **Emergency (priority-2) receipts** — state machine, durable retry scheduler
  (30 s floor, 3 h expire ceiling, 50-attempt hard cap), crash-recovery timers,
  ack, cancel, cancel-by-tag, 7-day query window, GC. `server/internal/receipts/`,
  `server/internal/api/receipts.go`, `server/internal/api/cancel.go`
- **Callback worker** — receipt-JSON webhook on ack, 60 s retry on non-2xx,
  SSRF allowlist (loopback/link-local/RFC1918/ULA blocked by default).
  `server/internal/callbacks/worker.go`
- **Realtime hub** — WebSocket and SSE with `since`-cursor replay, 45 s
  keepalive, device registration (`POST /1/devices/login.json`, SHA-256 secret).
  `server/internal/hub/`
- **Delivery channels** — WS/SSE (first-class), optional FCM v1 (env-gated),
  UnifiedPush distributor. `server/internal/fcm/`, `server/internal/up/`
- **Quiet hours** — server-side hold for `priority <= 0`, `priority >= 1`
  bypasses. `server/internal/quiethours/`
- **Subscriptions** — codes + dynamic per-app keys + migrate.
  `server/internal/api/subscriptions.go`
- **End-to-end encryption** — opaque storage of GZIP/AES-256-CBC/HMAC fields;
  server never decrypts. `server/internal/e2ee/`
- **Validate & sounds** — `POST /1/users/validate.json`, `GET /1/sounds.json`
  (23 built-in sounds). `server/internal/api/validate.go`,
  `server/internal/api/sounds.go`
- **Observability** — `GET /metrics` (Prometheus) + structured request logging.
  `server/internal/metrics/`
- **Retention & shutdown** — 30-day message retention, 3-day attachment-BLOB
  retention, TTL discard, graceful shutdown with WAL checkpoint.
  `server/internal/retention/`
- **Embedded dashboard** — Next.js static export served at `/admin/` via
  `go:embed` (apps, quota, live SSE message view, receipts, quiet-hours).
  `server/internal/webmount/`, `web/`
- **Clients** — Android (WS/FCM/UnifiedPush transports, emergency channel with
  Android 14 full-screen-intent flow, ack outbox), Tauri 2 desktop (direct WS,
  dedup, ack). `android/`, `desktop/`
- **Dual database** — SQLite (default, pure-Go `modernc.org/sqlite`) and
  Postgres (`pgx/v5`) behind the same store interfaces. See
  [docs/POSTGRES.md](docs/POSTGRES.md).

## Documentation

- [Getting started](docs/getting-started.md)
- [Configuration](docs/configuration.md)
- [HTTP API reference](docs/api.md)
- [Clients (Android, desktop, dashboard)](docs/clients.md)
- [Self-hosting (TLS, backups, Postgres)](docs/self-hosting.md)
- [Postgres backend](docs/POSTGRES.md)
- [Pushover API compatibility matrix](docs/API-COMPAT.md)

## Project layout

```text
server/    Go server (single binary): API, hub, receipts, store, dashboard embed
android/   native Android client (WS / FCM / UnifiedPush)
desktop/   Tauri 2 desktop client (direct WS)
web/       Next.js static-export admin dashboard (embedded into the binary)
deploy/    docker-compose and deployment assets
docs/      documentation set
```

## License

Licensed under the [Apache License 2.0](LICENSE).
