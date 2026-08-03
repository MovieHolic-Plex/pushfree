-- 0004_timers.up.sql
-- Durable timer engine support (todo 22).
--
-- The timers table itself (fire_at, kind, receipt_id, payload, claimed_at)
-- was created in 0001_init; this migration adds ONLY the index the
-- crash-recovery scan needs, without touching the table definition or any
-- row data. Todo 22 owns this migration and the internal/timers engine; the
-- base schema stays owned by todo 5.
--
-- On startup the engine must find every timer that a crashed worker CLAIMED
-- but never DELETED (fire-then-delete semantics: a surviving row with a
-- non-null claimed_at is an orphan from a kill -9 between claim and fire).
-- RecoverOrphanedClaims resets those rows' claimed_at to NULL so the normal
-- due-poll reclaims and fires them exactly once. The existing
-- idx_timers_due partial index covers the WHERE claimed_at IS NULL path;
-- this complementary partial index covers the WHERE claimed_at IS NOT NULL
-- path so the recovery scan stays cheap as the table grows.

CREATE INDEX IF NOT EXISTS idx_timers_orphaned ON timers(id) WHERE claimed_at IS NOT NULL;
