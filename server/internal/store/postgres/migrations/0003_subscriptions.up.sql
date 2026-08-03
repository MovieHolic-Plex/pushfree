-- 0003_subscriptions.up.sql
-- Postgres parity with SQLite 0003_subscriptions (todo 12). See the SQLite
-- migration for the dynamic per-(app, user) key semantics.

CREATE TABLE subscriptions (
    id                BIGSERIAL PRIMARY KEY,
    app_id            BIGINT NOT NULL REFERENCES apps(id),
    owner_user_id     BIGINT NOT NULL REFERENCES users(id),
    subscription_code TEXT NOT NULL UNIQUE CHECK(length(subscription_code)=30),
    title             TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_subscriptions_app ON subscriptions(app_id);

CREATE TABLE subscription_keys (
    id              BIGSERIAL PRIMARY KEY,
    subscription_id BIGINT NOT NULL REFERENCES subscriptions(id),
    app_id          BIGINT NOT NULL REFERENCES apps(id),
    user_id         BIGINT NOT NULL REFERENCES users(id),
    subscribed_key  TEXT NOT NULL UNIQUE CHECK(length(subscribed_key)=30),
    created_at      TIMESTAMPTZ NOT NULL,
    UNIQUE(app_id, user_id)
);

CREATE INDEX idx_subkeys_key ON subscription_keys(subscribed_key);
