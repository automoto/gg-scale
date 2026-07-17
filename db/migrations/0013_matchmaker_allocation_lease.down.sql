DROP POLICY IF EXISTS matchmaker_matches_worker_select ON matchmaker_matches;
DROP INDEX IF EXISTS matchmaker_matches_unclaimed_allocation_expiry_idx;

ALTER TABLE matchmaker_matches
    DROP COLUMN IF EXISTS claimed_at,
    DROP COLUMN IF EXISTS allocation_id;
