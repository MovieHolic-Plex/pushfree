-- 0004_timers.down.sql
-- Reverse of 0004: drop the crash-recovery index only. The timers table
-- itself is created/dropped by 0001; this migration never touched it.
-- IF EXISTS keeps Down idempotent.
DROP INDEX IF EXISTS idx_timers_orphaned;
