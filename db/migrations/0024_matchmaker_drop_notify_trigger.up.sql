-- Drop the per-row AFTER INSERT trigger that ran pg_notify inside the enqueue
-- transaction. That took a cluster-global notify lock held through the commit
-- fsync, serializing matchmaker enqueues. Wakeups are now sent post-commit and
-- debounced by the application (internal/matchmaker: notifyEnqueued).
DROP TRIGGER IF EXISTS matchmaking_tickets_notify ON public.matchmaking_tickets;
DROP FUNCTION IF EXISTS public.notify_matchmaker_ticket();
