# Backup and restore

This guide covers backing up and restoring PushFree's durable state: the
message database (SQLite or Postgres), the configuration, and any TLS
material. Operational concerns (TLS, reverse proxy, graceful shutdown) are in
[self-hosting.md](self-hosting.md); every config key is in
[configuration.md](configuration.md).

What is stateful and must be backed up:

| Artifact            | Where it lives                                  | Notes                                                    |
| ------------------- | ----------------------------------------------- | -------------------------------------------------------- |
| SQLite database     | `db-file` (default `pushfree.db`)               | Messages, accounts, apps, receipts, groups, devices.     |
| Postgres database   | the `db-url` cluster                            | Same schema as SQLite.                                   |
| Configuration       | your `pushfree.toml` (and any `PUSHFREE_*` env) | Needed to restart with the same keys/secrets.            |
| TLS material        | `tls-cert-file` / `tls-key-file` paths           | Only if you run built-in TLS (otherwise the proxy owns these). |
| Session secret      | `auth-secret`                                   | Stable value -> sessions survive restore; without it, sessions reset. |

Application source is in git; logs are ephemeral (structured JSON to stderr —
capture/rotate with your process supervisor, not part of a restore).

---

## SQLite

### Backup (online, consistent)

The server runs in WAL mode with `busy_timeout=5000` and performs a WAL
checkpoint during graceful shutdown, so a clean stop leaves a consistent file.
For a hot backup without stopping the server, use SQLite's online backup,
which produces a consistent snapshot regardless of concurrent writes:

```sh
# Consistent copy via SQLite's online backup API (recommended).
sqlite3 /var/lib/pushfree/pushfree.db \
  "VACUUM INTO '/backup/pushfree-$(date +%F).db'"
```

Alternatively, take a coordinated filesystem snapshot of **all three** files
together while the server runs:

```sh
# LVM/ZFS/btrfs snapshot while the server runs (capture all three atomically):
#   pushfree.db + pushfree.db-wal + pushfree.db-shm
```

For continuous protection, [litestream](https://litestream.io/) streams WAL
changes to S3-compatible storage. Configure it against the `pushfree.db` file;
the server's WAL mode and `busy_timeout` are compatible. Restore is "replay to
a point in time" per litestream's docs.

### Restore (SQLite)

1. Stop the server (preferred) so no writer is attached:

   ```sh
   # SIGTERM triggers graceful shutdown + WAL checkpoint (bounded by shutdown-timeout)
   ```

2. Replace the database file(s) with the backup. If your backup is a single
   `VACUUM INTO` `.db`, place it at the `db-file` path:

   ```sh
   cp /backup/pushfree-2026-08-03.db /var/lib/pushfree/pushfree.db
   rm -f /var/lib/pushfree/pushfree.db-wal /var/lib/pushfree/pushfree.db-shm
   ```

   If your backup is a filesystem snapshot of the three files, restore all
   three together.

3. Re-apply your configuration (see [Configuration](#configuration) below).

4. Start the server and confirm health:

   ```sh
   curl -sf http://localhost:2586/health   # -> {"status":"ok"}
   ```

Migrations are idempotent and run automatically on startup; restoring an older
schema onto a newer binary is safe (the binary will migrate forward). There is
no automatic down-migration.

---

## Postgres

### Backup (logical dump)

Use `pg_dump` against the connection string you set as `db-url`:

```sh
# Plain-text logical dump (gzip-compressed, roles excluded unless you need them)
pg_dump --no-owner --clean --if-exists \
  "postgres://pushfree:secret@db:5432/pushfree?sslmode=require" \
  | gzip > /backup/pushfree-$(date +%F).sql.gz
```

`--clean --if-exists` makes the dump restorable onto a non-empty database by
dropping conflicting objects first. For physical/point-in-time backups use
your Postgres operator's tooling (WAL archiving, base backups, or a managed
snapshot) — that is outside PushFree's scope.

### Restore (Postgres)

1. (Optional) stop the PushFree server so no writer is attached during restore.
2. Create/empty the target database, then load the dump:

   ```sh
   createdb -h db -U pushfree pushfree       # if starting fresh
   gunzip -c /backup/pushfree-2026-08-03.sql.gz \
     | psql "postgres://pushfree:secret@db:5432/pushfree?sslmode=require"
   ```

3. Point `db-url` at the restored database (see
   [POSTGRES.md](POSTGRES.md) for backend switching).
4. Start the server; migrations run automatically on startup.

   ```sh
   curl -sf http://localhost:2586/health   # -> {"status":"ok"}
   ```

---

## Configuration

Back up the exact `pushfree.toml` you run with, **including** any secrets it
contains (`auth-secret`, and any TLS key paths' meaning). Store configuration
backups in a secrets store with access controls appropriate to the secrets
they hold — do not commit them to git (see [../SECURITY.md](../SECURITY.md)).

To restore, place the file and pass it with `-config <path>`, or re-apply the
`PUSHFREE_*` environment variables (env always wins over the file). An empty
config file is valid; all keys have defaults except where a pair is required
(`tls-cert-file` + `tls-key-file` must be both set or both empty).

Key things to preserve across a restore so users are not logged out and TLS
keeps working:

- `auth-secret` — stable value keeps existing session cookies valid.
- `tls-cert-file` / `tls-key-file` — restore the certificate and private key
  to the same paths (or update the paths).
- `base-url` — keep it stable so generated callback/redirect URLs do not
  change.
- `db-file` / `db-url` — point at the restored database.

---

## TLS material

If you run built-in TLS (`tls-cert-file` + `tls-key-file`), back up the
certificate and private key out-of-band in a secrets store. To restore, place
them at the configured paths (or update the paths) and restart. If you run
behind a TLS-terminating reverse proxy (recommended), the proxy owns the
certificates and they are not part of a PushFree backup.

---

## Testing a restore

Treat a restore like any other disaster-recovery procedure: verify it on a
non-production instance periodically.

```sh
# 1. Start a fresh instance against the restored DB + config.
# 2. Health:
curl -sf http://localhost:2586/health
# 3. Log in with a known account (sessions survive if auth-secret was restored):
curl -sc cookies.txt -X POST http://localhost:2586/v1/accounts/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"correct-horse"}'
# 4. Confirm data is present:
curl -sb cookies.txt http://localhost:2586/v1/apps
# 5. Send a test message end-to-end (see getting-started.md).
```

A successful restore leaves the server healthy, existing sessions valid (when
`auth-secret` was preserved), and historical messages/apps/receipts visible.
