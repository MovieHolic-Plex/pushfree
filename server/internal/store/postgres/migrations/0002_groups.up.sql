-- 0002_groups.up.sql
-- Postgres parity with SQLite 0002_groups (todo 9). See the SQLite migration
-- for the H1 alignment notes (group_key is the same 30-char format as
-- users.user_key so the send path resolves each opaquely).

CREATE TABLE groups (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id),
    group_key  TEXT NOT NULL UNIQUE CHECK(length(group_key)=30),
    name       TEXT NOT NULL,
    memo       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE group_members (
    group_id BIGINT NOT NULL REFERENCES groups(id),
    user_id  BIGINT NOT NULL REFERENCES users(id),
    PRIMARY KEY (group_id, user_id)
);
