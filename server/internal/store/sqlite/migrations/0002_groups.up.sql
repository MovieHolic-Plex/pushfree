-- 0002_groups.up.sql
-- Delivery groups (todo 9). A group is a named recipient set owned by a user.
-- group_key is exactly 30 chars [A-Za-z0-9] -- the SAME format as users.user_key,
-- so the send path cannot tell a user_key from a group_key at lookup time; the
-- store resolves each (ResolveRecipients in send_message.go).
--
-- H1 alignment: sending to a group produces ONE sends row (the API call) plus
-- one messages row per member, and one receipt for the send (not per member).

CREATE TABLE groups (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id),
    group_key  TEXT NOT NULL UNIQUE CHECK(length(group_key)=30),
    name       TEXT NOT NULL,
    memo       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE TABLE group_members (
    group_id INTEGER NOT NULL REFERENCES groups(id),
    user_id  INTEGER NOT NULL REFERENCES users(id),
    PRIMARY KEY (group_id, user_id)
);
