-- 0001_init.down.sql
-- MANUAL ROLLBACK script (todo 45): the startup runner applies UP migrations
-- only. To revert init, run this file by hand against the target database.
-- Drop order respects FKs (children first).
DROP TABLE IF EXISTS dlq;
DROP TABLE IF EXISTS callbacks;
DROP TABLE IF EXISTS timers;
DROP TABLE IF EXISTS quota_counters;
DROP TABLE IF EXISTS receipts;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS sends;
DROP TABLE IF EXISTS devices;
DROP TABLE IF EXISTS apps;
DROP TABLE IF EXISTS users;
