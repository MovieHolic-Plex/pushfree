-- 0001_init.up.sql
-- Postgres parity with the SQLite 0001_init schema (todo 5). Same logical
-- model (H1 "sends-parent": a receipt belongs to an API send, not to any per-
-- recipient message), same CHECK constraints, same identifier rules (30-char
-- user_key/token/receipt_id). Dialect differences only:
--   INTEGER PRIMARY KEY AUTOINCREMENT -> BIGSERIAL PRIMARY KEY   (int8 IDs)
--   INTEGER 0/1 flags                 -> BOOLEAN                (html/mono/enc)
--   TEXT instant columns              -> TIMESTAMPTZ             (native tz)
--   BLOB                              -> BYTEA                   (attachments)
-- The store layer (internal/store/store.go) exposes time.Time for instants and
-- bool for flags, so the Go interface is identical across both backends.

CREATE TABLE users (
    id          BIGSERIAL PRIMARY KEY,
    email       TEXT NOT NULL UNIQUE,
    pass_hash   TEXT NOT NULL,
    role        TEXT NOT NULL CHECK(role IN ('user','admin')),
    user_key    TEXT NOT NULL UNIQUE CHECK(length(user_key)=30),
    quiet_start TEXT,
    quiet_end   TEXT,
    quiet_tz    TEXT NOT NULL DEFAULT 'UTC',
    created_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE apps (
    id      BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    token   TEXT NOT NULL UNIQUE CHECK(length(token)=30),
    name    TEXT NOT NULL
);

CREATE TABLE devices (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id),
    device_id   TEXT NOT NULL UNIQUE,
    secret_hash TEXT NOT NULL,
    name        TEXT NOT NULL CHECK(length(name)<=25),
    model       TEXT NOT NULL DEFAULT '',
    os          TEXT NOT NULL DEFAULT '',
    fcm_token   TEXT
);

-- One API call = one row.
CREATE TABLE sends (
    id             BIGSERIAL PRIMARY KEY,
    app_id         BIGINT NOT NULL REFERENCES apps(id),
    sender_user_id BIGINT NOT NULL REFERENCES users(id),
    priority       INTEGER NOT NULL DEFAULT 0 CHECK(priority BETWEEN -2 AND 2),
    sound          TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    url_title      TEXT NOT NULL DEFAULT '',
    html           BOOLEAN NOT NULL DEFAULT FALSE,
    monospace      BOOLEAN NOT NULL DEFAULT FALSE,
    timestamp      BIGINT NOT NULL DEFAULT 0,
    ttl            BIGINT NOT NULL DEFAULT 0,
    tag            TEXT,
    encrypted      BOOLEAN NOT NULL DEFAULT FALSE,
    callback_url   TEXT,
    receipt_id     TEXT UNIQUE CHECK(receipt_id IS NULL OR length(receipt_id)=30),
    created_at     TIMESTAMPTZ NOT NULL
);

-- Per-recipient fan-out rows.
CREATE TABLE messages (
    id                BIGSERIAL PRIMARY KEY,
    send_id           BIGINT NOT NULL REFERENCES sends(id),
    recipient_user_id BIGINT NOT NULL REFERENCES users(id),
    device_filter     TEXT,
    delivered_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_messages_send_id     ON messages(send_id);
CREATE INDEX idx_messages_recipient_id ON messages(recipient_user_id, id);

CREATE TABLE attachments (
    id            BIGSERIAL PRIMARY KEY,
    send_id       BIGINT NOT NULL UNIQUE REFERENCES sends(id),
    content_type  TEXT NOT NULL,
    data          BYTEA NOT NULL,
    downloaded_at TIMESTAMPTZ
);

CREATE TABLE receipts (
    id                    TEXT PRIMARY KEY CHECK(length(id)=30),
    send_id               BIGINT NOT NULL UNIQUE REFERENCES sends(id),
    state                 TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','delivered','acknowledged','expired','canceled')),
    tag                   TEXT,
    retry_count           INTEGER NOT NULL DEFAULT 0,
    expires_at            TIMESTAMPTZ,
    acknowledged_at       TIMESTAMPTZ,
    acknowledged_by       TEXT,
    acknowledged_by_device TEXT,
    last_delivered_at     TIMESTAMPTZ,
    called_back_at        TIMESTAMPTZ,
    expired_at            TIMESTAMPTZ,
    canceled_at           TIMESTAMPTZ
);

CREATE TABLE quota_counters (
    user_id BIGINT NOT NULL REFERENCES users(id),
    period  TEXT NOT NULL, /* YYYY-MM */
    count   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, period)
);

CREATE TABLE timers (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL CHECK(kind IN ('retry','expire','callback','quiethours')),
    receipt_id TEXT REFERENCES receipts(id),
    fire_at    TIMESTAMPTZ NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    claimed_at TIMESTAMPTZ
);
CREATE INDEX idx_timers_due ON timers(fire_at) WHERE claimed_at IS NULL;

CREATE TABLE callbacks (
    id              BIGSERIAL PRIMARY KEY,
    receipt_id      TEXT NOT NULL REFERENCES receipts(id),
    url             TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending',
    next_attempt_at TIMESTAMPTZ,
    attempts        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE dlq (
    id          BIGSERIAL PRIMARY KEY,
    callback_id BIGINT NOT NULL REFERENCES callbacks(id),
    last_error  TEXT NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0
);
