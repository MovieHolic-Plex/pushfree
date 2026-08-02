-- 0001_init.up.sql
-- First schema: the H1 "sends-parent" model where a receipt belongs to an
-- API send, not to a per-recipient message. All temporal columns are RFC3339
-- TEXT (see internal/store/store.go). Identifiers (user_key, token,
-- receipt_id) are exactly 30 chars.

CREATE TABLE users (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    email       TEXT NOT NULL UNIQUE,
    pass_hash   TEXT NOT NULL,
    role        TEXT NOT NULL CHECK(role IN ('user','admin')),
    user_key    TEXT NOT NULL UNIQUE CHECK(length(user_key)=30),
    quiet_start TEXT,
    quiet_end   TEXT,
    quiet_tz    TEXT NOT NULL DEFAULT 'UTC',
    created_at  TEXT NOT NULL
);

CREATE TABLE apps (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id),
    token   TEXT NOT NULL UNIQUE CHECK(length(token)=30),
    name    TEXT NOT NULL
);

CREATE TABLE devices (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id),
    device_id   TEXT NOT NULL UNIQUE,
    secret_hash TEXT NOT NULL,
    name        TEXT NOT NULL CHECK(length(name)<=25),
    model       TEXT NOT NULL DEFAULT '',
    os          TEXT NOT NULL DEFAULT '',
    fcm_token   TEXT
);

-- One API call = one row.
CREATE TABLE sends (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id         INTEGER NOT NULL REFERENCES apps(id),
    sender_user_id INTEGER NOT NULL REFERENCES users(id),
    priority       INTEGER NOT NULL DEFAULT 0 CHECK(priority BETWEEN -2 AND 2),
    sound          TEXT NOT NULL DEFAULT '',
    title          TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    url_title      TEXT NOT NULL DEFAULT '',
    html           INTEGER NOT NULL DEFAULT 0,
    monospace      INTEGER NOT NULL DEFAULT 0,
    timestamp      INTEGER NOT NULL DEFAULT 0,
    ttl            INTEGER NOT NULL DEFAULT 0,
    tag            TEXT,
    encrypted      INTEGER NOT NULL DEFAULT 0,
    callback_url   TEXT,
    receipt_id     TEXT UNIQUE CHECK(receipt_id IS NULL OR length(receipt_id)=30),
    created_at     TEXT NOT NULL
);

-- Per-recipient fan-out rows.
CREATE TABLE messages (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    send_id          INTEGER NOT NULL REFERENCES sends(id),
    recipient_user_id INTEGER NOT NULL REFERENCES users(id),
    device_filter    TEXT,
    delivered_at     TEXT,
    created_at       TEXT NOT NULL
);
CREATE INDEX idx_messages_send_id     ON messages(send_id);
CREATE INDEX idx_messages_recipient_id ON messages(recipient_user_id, id);

CREATE TABLE attachments (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    send_id       INTEGER NOT NULL UNIQUE REFERENCES sends(id),
    content_type  TEXT NOT NULL,
    data          BLOB NOT NULL,
    downloaded_at TEXT
);

CREATE TABLE receipts (
    id                   TEXT PRIMARY KEY CHECK(length(id)=30),
    send_id              INTEGER NOT NULL UNIQUE REFERENCES sends(id),
    state                TEXT NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','delivered','acknowledged','expired','canceled')),
    tag                  TEXT,
    retry_count          INTEGER NOT NULL DEFAULT 0,
    expires_at           TEXT,
    acknowledged_at      TEXT,
    acknowledged_by      TEXT,
    acknowledged_by_device TEXT,
    last_delivered_at    TEXT,
    called_back_at       TEXT,
    expired_at           TEXT,
    canceled_at          TEXT
);

CREATE TABLE quota_counters (
    user_id INTEGER NOT NULL REFERENCES users(id),
    period  TEXT NOT NULL, /* YYYY-MM */
    count   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, period)
);

CREATE TABLE timers (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL CHECK(kind IN ('retry','expire','callback','quiethours')),
    receipt_id TEXT REFERENCES receipts(id),
    fire_at    TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    claimed_at TEXT
);
CREATE INDEX idx_timers_due ON timers(fire_at) WHERE claimed_at IS NULL;

CREATE TABLE callbacks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    receipt_id      TEXT NOT NULL REFERENCES receipts(id),
    url             TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'pending',
    next_attempt_at TEXT,
    attempts        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE dlq (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    callback_id INTEGER NOT NULL REFERENCES callbacks(id),
    last_error  TEXT NOT NULL,
    at          TEXT NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0
);
