-- 0004_timers.up.sql
-- Postgres parity with SQLite 0004_timers (todo 22). Adds ONLY the partial
-- index the crash-recovery scan needs; the timers table itself is owned by
-- 0001_init. The orphaned-claim recovery scan looks for surviving rows with a
-- non-null claimed_at (a kill -9 between ClaimDue and Delete).

CREATE INDEX IF NOT EXISTS idx_timers_orphaned ON timers(id) WHERE claimed_at IS NOT NULL;
