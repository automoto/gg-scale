-- One active matchmaking ticket per player per project, step 2 of 2: the
-- partial unique index that enforces it. A player who is already queued (or
-- mid-negotiation — claimed tickets stay 'queued') cannot open a second
-- ticket; the API turns the resulting unique violation into a 409. Built
-- CONCURRENTLY so the build does not take a blocking lock on the live
-- matchmaking_tickets table (0018 has already collapsed pre-existing
-- duplicates). CONCURRENTLY cannot run inside a transaction, so this migration
-- must contain exactly one statement. IF NOT EXISTS lets a retry skip an index
-- left behind by an interrupted build.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS matchmaking_tickets_one_active_idx
    ON matchmaking_tickets (tenant_id, project_id, player_id)
    WHERE status = 'queued';
