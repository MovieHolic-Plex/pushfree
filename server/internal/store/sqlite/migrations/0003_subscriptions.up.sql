-- 0003_subscriptions.up.sql
-- Subscription codes with dynamic per-app keys (todo 12).
--
-- A subscription is a discoverable "subscribe to my app" channel owned by an
-- app. A subscriber approves via the session-authenticated
-- /1/subscriptions/authorize endpoint, which mints a per-(app, user) dynamic
-- subscribed_user_key. That key is DIFFERENT per app but STABLE per app+user
-- (re-approving the same pair returns the same key), and it resolves
-- transparently like a user_key in the send path (ResolveRecipients in
-- send_message.go): sending to it fans out one message row to the underlying
-- user.
--
-- Migrating a subscription to a new app (migrate.json) re-parents the channel
-- and regenerates every subscriber key (old keys become invalid, new keys are
-- minted for the destination app), preserving each user_id so delivery still
-- reaches the same recipient.

CREATE TABLE subscriptions (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id            INTEGER NOT NULL REFERENCES apps(id),
    owner_user_id     INTEGER NOT NULL REFERENCES users(id),
    subscription_code TEXT NOT NULL UNIQUE CHECK(length(subscription_code)=30),
    title             TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL
);

CREATE INDEX idx_subscriptions_app ON subscriptions(app_id);

-- Per-(app, user) dynamic subscriber keys. Each row is one approval. The
-- UNIQUE(app_id, user_id) constraint makes approval idempotent and stable:
-- re-approving the same app+user returns the SAME key rather than minting a
-- new one. subscribed_key resolves to user_id in the send path, so it behaves
-- exactly like a user_key (same 30-char [A-Za-z0-9] format). subscription_id
-- records the channel through which the approval happened.
CREATE TABLE subscription_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    subscription_id INTEGER NOT NULL REFERENCES subscriptions(id),
    app_id          INTEGER NOT NULL REFERENCES apps(id),
    user_id         INTEGER NOT NULL REFERENCES users(id),
    subscribed_key  TEXT NOT NULL UNIQUE CHECK(length(subscribed_key)=30),
    created_at      TEXT NOT NULL,
    UNIQUE(app_id, user_id)
);

CREATE INDEX idx_subkeys_key ON subscription_keys(subscribed_key);
