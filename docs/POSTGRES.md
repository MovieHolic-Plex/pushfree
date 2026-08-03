# Postgres backend

pushfree ships with **two** storage backends behind the same
`internal/store` interfaces:

- **SQLite** (default) — `modernc.org/sqlite`, pure Go, a single file. Best for
  single-node self-hosting up to ~10k messages/month.
- **Postgres** — `pgx/v5`, for operators who already run Postgres or who need a
  shared/managed database.

Both are exercised in CI: see `.github/workflows/server-db.yml`
(sqlite + postgres lanes).

## Schema parity

The Postgres schema (`server/internal/store/postgres/migrations/0001-0004`)
is a logical parity port of the SQLite schema
(`server/internal/store/sqlite/migrations/0001-0004`). The Dialect differences
only reflect idiomatic Postgres types:

| SQLite                              | Postgres                  | Meaning                     |
| ----------------------------------- | ------------------------- | --------------------------- |
| `INTEGER PRIMARY KEY AUTOINCREMENT` | `BIGSERIAL PRIMARY KEY`   | auto-increment id (int8)    |
| `INTEGER` (0/1 flags)               | `BOOLEAN`                 | html/monospace/encrypted    |
| `TEXT` (RFC3339 instant)            | `TIMESTAMPTZ`             | created_at, fire_at, *_at   |
| `BLOB`                              | `BYTEA`                   | attachments.data            |
| `?` placeholders                    | `$N` placeholders         | parameter binding           |
| `res.LastInsertId()`                | `INSERT ... RETURNING id` | generated id retrieval      |
| `INSERT OR IGNORE`                  | `ON CONFLICT DO NOTHING`  | idempotent group-member add |
| single-writer claim                 | `FOR UPDATE SKIP LOCKED`  | concurrent timer claim      |

The `store.Repos` Go interface is identical across both backends: IDs are
`int64`, instants are `time.Time`, nullable TEXT is `""`, nullable instants are
`*time.Time`.

## Switching to Postgres

Set `db-url` (a pgx connection string) in your config or via the
`PUSHFREE_DB_URL` environment override. When `db-url` is set the server uses
the Postgres backend; otherwise it uses `db-file` (SQLite).

```toml
# pushfree.toml
db-url = "postgres://pushfree:secret@localhost:5432/pushfree?sslmode=disable"
```

At startup the server runs all pending **up** migrations automatically and
idempotently (version-pinned in the `schema_migrations` table). There is no
automatic down-migration — see **Rollback** below.

### Migrating data from SQLite (one-time)

There is no built-in SQLite→Postgres data copy. To move an existing SQLite
database:

1. Stop the server.
2. Export each table from SQLite in CSV/SQL (e.g. `sqlite3 pushfree.db .dump`).
3. Load into Postgres **after** the schema migrations have run once (start the
   server against the empty Postgres DB, then stop it), taking care of the
   `BIGSERIAL` sequence values (`SELECT setval(...)` for each table after the
   copy so new rows do not collide).
4. Restart the server against Postgres.

## Rollback (manual)

Down migrations are **not** applied automatically at startup — an accidental
revert must never destroy production data. Each migration has a matching
`*.down.sql` rollback script in
`server/internal/store/postgres/migrations/`:

- `0001_init.down.sql` — drops all init tables (children before parents).
- `0002_groups.down.sql` — drops `group_members`, `groups`.
- `0003_subscriptions.down.sql` — drops `subscription_keys`, `subscriptions`.
- `0004_timers.down.sql` — drops the orphaned-claim index.

To revert, apply the relevant `*.down.sql` files by hand in **descending**
version order (newest first) against the target database, then remove the
matching rows from `schema_migrations`. Example, reverting just 0004:

```sh
psql "$DB_URL" \
  -f server/internal/store/postgres/migrations/0004_timers.down.sql \
  -c "DELETE FROM schema_migrations WHERE version = 4;"
```

Always take a `pg_dump` backup before rolling back.

## Scope note (engine interfaces)

This backend implements every repository in `store.Repos` (users, apps,
devices, sends, messages, attachments, receipts, quota, timers, callbacks,
ingests, groups, subscriptions). The auxiliary engine interfaces used by the
receipts/timer/callback/retention subsystems (`receipts.CancelStore`, the timer
engine's `Delete`/`ResetOrphanedClaims`, the retention sweeper, the callback
worker) currently have SQLite implementations only and are tracked as
follow-ups for full Postgres runtime parity. The store boundary itself is
backend-complete.
