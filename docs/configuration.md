# Configuration

pushfree resolves configuration from three sources, in order of increasing
precedence:

1. Built-in defaults.
2. An optional TOML file (path passed via `pushfree -config <path>`).
3. `PUSHFREE_*` environment variables (env always wins).

A copy with every key and its default lives at
`server/pushfree.example.toml`. An empty config file is valid. The values
below are the authoritative defaults, sourced from
`server/internal/config/config.go`.

For every dashed TOML key there is an underscored environment override:
`listen-addr` -> `PUSHFREE_LISTEN_ADDR`. A list-valued env var
(`callback-allowed-hosts`) is comma-separated.

## All keys

| Key                          | Env override                          | Default       | Notes                                                                 |
| ---------------------------- | ------------------------------------- | ------------- | --------------------------------------------------------------------- |
| `version`                    | `PUSHFREE_VERSION`                    | `1`           | Config schema version. Must match the binary or startup is refused.   |
| `listen-addr`                | `PUSHFREE_LISTEN_ADDR`                | `:2586`       | Address the HTTP(S) server binds.                                     |
| `tls-cert-file`              | `PUSHFREE_TLS_CERT_FILE`              | `""`          | Path to a TLS certificate. Set BOTH to serve HTTPS directly.          |
| `tls-key-file`               | `PUSHFREE_TLS_KEY_FILE`               | `""`          | Path to the matching TLS private key.                                 |
| `base-url`                   | `PUSHFREE_BASE_URL`                   | `""`          | Public base URL clients use to reach the server.                      |
| `db-file`                    | `PUSHFREE_DB_FILE`                    | `pushfree.db` | SQLite database file (pure-Go driver, no cgo).                        |
| `db-url`                     | `PUSHFREE_DB_URL`                     | `""`          | Postgres connection string. When set, Postgres is used instead.       |
| `fcm-credentials-file`       | `PUSHFREE_FCM_CREDENTIALS_FILE`       | `""`          | Path to an FCM v1 service-account JSON. Omit to disable FCM.          |
| `keepalive-interval`         | `PUSHFREE_KEEPALIVE_INTERVAL`         | `45s`         | WebSocket/SSE keepalive cadence.                                      |
| `quota-monthly`              | `PUSHFREE_QUOTA_MONTHLY`              | `10000`       | Per-user monthly send quota (per recipient, not per attempt).         |
| `messages-retention`         | `PUSHFREE_MESSAGES_RETENTION`         | `720h`        | How long delivered messages are kept (720 h = 30 days).               |
| `attachment-retention`       | `PUSHFREE_ATTACHMENT_RETENTION`       | `72h`         | How long an undownloaded attachment BLOB is kept (72 h = 3 days).     |
| `sweeper-interval`           | `PUSHFREE_SWEEPER_INTERVAL`           | `1h`          | How often the retention sweeper runs (also runs once at startup).     |
| `shutdown-timeout`           | `PUSHFREE_SHUTDOWN_TIMEOUT`           | `10s`         | Budget for the graceful-shutdown tail (WAL checkpoint).               |
| `shutdown-on-stdin-eof`      | `PUSHFREE_SHUTDOWN_ON_STDIN_EOF`      | `false`       | Windows/testing aid: closing stdin triggers graceful shutdown.        |
| `callback-allowed-hosts`     | `PUSHFREE_CALLBACK_ALLOWED_HOSTS`     | `[]`          | Hosts allowed as receipt callback targets (private ranges blocked).   |
| `auth-secret`                | `PUSHFREE_AUTH_SECRET`                | `""`          | Secret signing session cookies. Empty -> random per-process secret.   |

## Validation rules

These are enforced at startup in `config.validate()`:

- `version` must equal the binary's expected schema version (currently `1`).
  A mismatch is a hard error naming the field.
- `tls-cert-file` and `tls-key-file` must **both** be set or **both** be empty.
  Setting only one is a startup error.
- When neither TLS key is set the server serves plain HTTP and logs a warning
  recommending a TLS-terminating reverse proxy.

A malformed env value for an integer/bool field (e.g. `PUSHFREE_VERSION=abc`)
is a startup error rather than a silent fallback to the file/default value.

## Durations

Duration fields (`keepalive-interval`, `messages-retention`,
`attachment-retention`, `sweeper-interval`, `shutdown-timeout`) are parsed with
Go's `time.ParseDuration`, so values like `45s`, `720h`, `1h`, `10s` are valid.
An empty duration string parses to zero (a valid "disabled" value for the
optional retention windows). Source: `server/internal/retention/durations.go`.

## Choosing a database

- **SQLite** (default) — single file, zero ops, good up to roughly 10,000
  messages/month on one node. Set `db-file`.
- **Postgres** — for operators who already run Postgres or need a shared DB.
  Set `db-url` to a pgx connection string. The schema migrates automatically
  on startup. See [POSTGRES.md](POSTGRES.md) for the full migration/rollback
  guide.

The two backends share one Go interface (`store.Repos`); switching is purely a
config change.

## Choosing delivery channels

- **WebSocket / SSE** — always available; the first-class transports used by
  the desktop and Android apps.
- **FCM v1** — optional. Provide `fcm-credentials-file` (a service-account
  JSON) to enable it; with no file the server starts cleanly and FCM stays
  disabled.
- **UnifiedPush** — always available as a distributor endpoint (see
  [api.md](api.md#unifiedpush-distributor)). A connector app registers an
  endpoint; no server config is required.

## Receipt callback egress

`callback-allowed-hosts` controls where receipt callbacks (priority-2 ack
webhooks) may be delivered. By default the callback worker blocks loopback,
link-local (`169.254/16`), RFC1918, and ULA addresses to prevent SSRF. List
the public hosts you trust here to permit them. Redirect targets are
re-checked against the same rules. Source:
`server/internal/callbacks/worker.go`.
