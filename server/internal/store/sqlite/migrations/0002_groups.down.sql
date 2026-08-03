-- 0002_groups.down.sql
-- Reverse of 0002: members first (FK -> groups), then groups. IF EXISTS keeps
-- Down idempotent.
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
