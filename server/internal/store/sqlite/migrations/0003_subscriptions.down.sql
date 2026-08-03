-- 0003_subscriptions.down.sql
-- Reverse of 0003: child first (FK -> subscriptions), then the channel table.
-- IF EXISTS keeps Down idempotent.
DROP TABLE IF EXISTS subscription_keys;
DROP TABLE IF EXISTS subscriptions;
