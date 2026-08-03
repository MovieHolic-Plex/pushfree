# Self-hosting

This covers running pushfree in production: TLS termination, the database,
backups, reverse-proxying, and graceful operation. The config keys referenced
here are documented fully in [configuration.md](configuration.md) and sourced
from `server/internal/config/config.go`.

## Listening and TLS

pushfree listens on `:2586` by default (`listen-addr`). You have two TLS
options; pick one.

### Option A: built-in TLS

Set **both** `tls-cert-file` and `tls-key-file`. Setting only one is a startup
error (`config.validate()` enforces all-or-nothing). The server then serves
HTTPS directly.

```toml
tls-cert-file = "/etc/pushfree/fullchain.pem"
tls-key-file  = "/etc/pushfree/privkey.pem"
```

### Option B: reverse proxy (recommended)

Leave both TLS keys empty. The server serves plain HTTP and logs a warning;
put it behind a TLS-terminating reverse proxy. Bind the listener to a local
address so only the proxy can reach it.

```toml
listen-addr = "127.0.0.1:2586"
```

Minimal Caddy (automatic HTTPS):

```caddyfile
push.example.com {
    reverse_proxy 127.0.0.1:2586
}
```

Minimal nginx (WebSocket/SSE aware — the realtime transports are long-lived,
so upgrade headers and disabled buffering matter):

```nginx
server {
    listen 443 ssl http2;
    server_name push.example.com;
    ssl_certificate     /etc/ssl/pushfree/fullchain.pem;
    ssl_certificate_key /etc/ssl/pushfree/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:2586;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;     # WebSocket
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_buffering off;                        # SSE streaming
        proxy_read_timeout 75s;                     # > 45s keepalive
    }
}
```

> The server's 45 s keepalive is deliberately under nginx's default 60 s idle
> timeout and common carrier NAT windows (60-120 s). Keep your proxy's read
> timeout above 45 s. Source: `server/internal/hub/hub.go`.

## Session secret

Set `auth-secret` to a stable, high-entropy value so logins survive restarts.
With none set, the server generates a random per-process secret, logs a
warning, and **all sessions invalidate on every restart**. Source:
`server/cmd/pushfree/main.go`.

## Database

### SQLite (default)

Default backend: a single file (`db-file`, default `pushfree.db`) using the
pure-Go `modernc.org/sqlite` driver (no cgo, WAL mode, `busy_timeout=5000`).
Good for single-node deployments up to roughly 10,000 messages/month. Back up
the file (see below).

### Postgres

For shared/managed databases, set `db-url` to a pgx connection string. When
set, the server uses the Postgres backend; otherwise it uses `db-file`.
Migrations run automatically and idempotently on startup (version-pinned in
`schema_migrations`). There is no automatic down-migration.

```toml
db-url = "postgres://pushfree:secret@db:5432/pushfree?sslmode=require"
```

Full schema-parity notes, SQLite->Postgres data migration, and rollback
instructions are in [POSTGRES.md](POSTGRES.md).

## Backups

### SQLite

The server runs a WAL checkpoint during graceful shutdown, so a clean stop
leaves a consistent file. To take a hot backup without stopping, use one of:

```sh
# Consistent copy via SQLite's online backup (recommended).
sqlite3 pushfree.db "VACUUM INTO '/backup/pushfree-$(date +%F).db'"

# Or filesystem snapshot of pushfree.db + pushfree.db-wal + pushfree.db-shm
# together (e.g. LVM/ZFS snapshot while the server runs).
```

For continuous protection, [litestream](https://litestream.io/) can stream WAL
changes to S3-compatible storage. Configure it against `pushfree.db`; the
server's own `busy_timeout` and WAL mode are compatible.

### Postgres backup

### Logs

The server logs structured JSON to **stderr**. Capture and rotate it with your
process supervisor (`systemd` `journald`, Docker logging drivers, etc.). Every
request line carries `request_id`, `method`, `path`, `status`, `duration_ms`.

## Docker

A multi-stage image and a compose file are provided for container deployment.

- Image build: `server/Dockerfile` — multi-stage `golang:1.26-alpine` ->
  `gcr.io/distroless/static-debian12:nonroot`, `CGO_ENABLED=0`, runs as the
  non-root user (uid 65532), exposes `2586`, and ships a real in-container
  `HEALTHCHECK` (a tiny compiled `pf-healthcheck` helper that does
  `GET /health`, since distroless has no shell/curl). The resulting image is
  ~21 MB.
- Compose: `deploy/docker-compose.yml` — the `pushfree` service plus an
  **optional** Postgres service (`postgres:17-alpine`, enabled via the
  `postgres` profile), with named volumes `pushfree-data` (the SQLite DB) and
  `pg-data`, and the config bind-mounted read-only from
  `deploy/pushfree.example.toml`.

Build and run (from the repo root):

```sh
docker build -t pushfree server/
docker compose -f deploy/docker-compose.yml up -d
curl -sf localhost:2586/health   # -> {"status":"ok"}
```

Or let compose build the image itself: `docker compose -f
 deploy/docker-compose.yml up -d --build`.

The runtime configuration is exactly the `pushfree.toml` / `PUSHFREE_*` keys
documented in [configuration.md](configuration.md). The image sets
`PUSHFREE_DB_FILE=/var/lib/pushfree/pushfree.db` so the SQLite database lands
in the persistent volume, and the compose file passes `PUSHFREE_AUTH_SECRET`
(set it so sessions survive restarts) and `PUSHFREE_DB_URL` (to switch to the
Postgres profile) through from the environment.

### Postgres via compose

```sh
docker compose -f deploy/docker-compose.yml --profile postgres up -d
# with PUSHFREE_DB_URL=postgres://pushfree:pushfree@pushfree-postgres:5432/pushfree?sslmode=disable
```

See [POSTGRES.md](POSTGRES.md) for backend switching and migration.

### TLS and the container healthcheck

The image's `HEALTHCHECK` probes `GET /health` over **plain HTTP** (the
recommended mode is HTTP behind a TLS-terminating reverse proxy). If you enable
built-in TLS (`tls-cert-file`/`tls-key-file`), disable the container
healthcheck (`HEALTHCHECK NONE` / remove the compose `healthcheck:` block) and
probe over HTTPS externally, or the probe will mark the container unhealthy.
See `deploy/pushfree.example.toml` for the cert-mount notes.

## Metrics and health

- `GET /health` returns `{"status":"ok"}` — use it as a container/pod
  `HEALTHCHECK` / liveness probe.
- `GET /metrics` exposes Prometheus text. It is **not** authenticated; bind
  the listener to a private address or scrape via the loopback only.

## Graceful shutdown

On `SIGTERM`/`SIGINT` the server drains in-flight HTTP requests, signals the
hub to close live connections, waits for the sweeper and callback worker to
stop touching the database, and runs a WAL checkpoint — all bounded by
`shutdown-timeout` (default 10 s). On Windows (where a POSIX `SIGTERM` cannot
be delivered to a console child), set `shutdown-on-stdin-eof = true` to make
closing stdin trigger the same graceful shutdown. Source:
`server/cmd/pushfree/main.go`.

## Retention

- Delivered messages: 30 days (`messages-retention`, default `720h`).
- Undownloaded attachment BLOBs: 3 days (`attachment-retention`, default `72h`)
  — the row survives so clients still see the attachment existed.
- TTL: messages whose `ttl` elapsed and were never delivered are discarded.
- Receipts: queryable for 7 days, then garbage-collected.

The sweeper runs every `sweeper-interval` (default 1 h) and once immediately
at startup. Source: `server/internal/retention/sweeper.go`.
