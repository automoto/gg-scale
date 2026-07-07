-- The matchmaker worker scans, claims, commits, and sweeps tickets across
-- tenants from a privileged loop that has no request tenant, but 0046 put
-- matchmaking_tickets under FORCE RLS with only the tenant-GUC isolation
-- policy — under the ggscale_app role every worker query silently matched
-- zero rows. Mirror the api_keys_bootstrap pattern (0010): additional
-- permissive policies admit GUC-less sessions, but only for the commands
-- the worker actually issues (no INSERT — ticket and match inserts always
-- run tenant-scoped). Request paths always run with app.tenant_id set, so
-- per-tenant isolation is unchanged for them.
CREATE POLICY matchmaking_tickets_worker_select ON matchmaking_tickets
    FOR SELECT
    USING (nullif(current_setting('app.tenant_id', true), '') IS NULL);

CREATE POLICY matchmaking_tickets_worker_update ON matchmaking_tickets
    FOR UPDATE
    USING (nullif(current_setting('app.tenant_id', true), '') IS NULL)
    WITH CHECK (nullif(current_setting('app.tenant_id', true), '') IS NULL);

-- Retention GC (River job) deletes terminal tickets cross-tenant.
CREATE POLICY matchmaking_tickets_worker_delete ON matchmaking_tickets
    FOR DELETE
    USING (nullif(current_setting('app.tenant_id', true), '') IS NULL);

-- Match results: only the retention GC runs GUC-less (deletes expired
-- rows); reads and inserts are always tenant-scoped.
CREATE POLICY matchmaker_matches_worker_delete ON matchmaker_matches
    FOR DELETE
    USING (nullif(current_setting('app.tenant_id', true), '') IS NULL);
