-- Dropped CONCURRENTLY to match the concurrent build; single statement, since
-- DROP INDEX CONCURRENTLY also cannot run inside a transaction.
DROP INDEX CONCURRENTLY IF EXISTS matchmaking_tickets_one_active_idx;
