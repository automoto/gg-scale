ALTER TABLE matchmaker_matches
    ADD COLUMN allocation_id bigint REFERENCES game_server_allocations(id) ON DELETE SET NULL,
    ADD COLUMN claimed_at timestamptz;

CREATE INDEX matchmaker_matches_unclaimed_allocation_expiry_idx
    ON matchmaker_matches (expires_at, id)
    WHERE allocation_id IS NOT NULL AND claimed_at IS NULL;

CREATE POLICY matchmaker_matches_worker_select
    ON matchmaker_matches
    FOR SELECT
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);
