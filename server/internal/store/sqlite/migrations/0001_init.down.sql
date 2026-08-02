-- 0001_init.down.sql
-- Reverse dependency order. IF EXISTS so Down is idempotent on a fresh DB.
DROP TABLE IF EXISTS dlq;
DROP TABLE IF EXISTS callbacks;
DROP INDEX IF EXISTS idx_timers_due;
DROP TABLE IF EXISTS timers;
DROP TABLE IF EXISTS quota_counters;
DROP TABLE IF EXISTS receipts;
DROP TABLE IF EXISTS attachments;
DROP INDEX IF EXISTS idx_messages_recipient_id;
DROP INDEX IF EXISTS idx_messages_send_id;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS sends;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS users;
