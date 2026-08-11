-- Per-project player deletion with a grace period: an explicit request disables
-- the player and stamps delete_requested_at; the player_delete_purge sweep
-- hard-deletes the row once the grace window passes. Tenants/projects can adopt
-- the same delete_requested_at + sweep pattern later.
ALTER TABLE project_players
    ADD COLUMN delete_requested_at timestamptz;

-- The sweep scans per tenant for due requests; pending rows are rare, so the
-- index stays partial.
CREATE INDEX project_players_delete_requested_idx
    ON project_players (tenant_id, delete_requested_at)
    WHERE delete_requested_at IS NOT NULL;

-- Audit rows must survive the purge: the actor goes NULL instead of blocking
-- the DELETE (the old constraint had no ON DELETE action). audit_log_actor_idx
-- already filters actor_user_id IS NOT NULL. NOT VALID skips revalidation:
-- every row satisfied the stricter old constraint.
ALTER TABLE audit_log
    DROP CONSTRAINT audit_log_actor_user_id_fkey;
ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_actor_user_id_fkey
        FOREIGN KEY (actor_user_id) REFERENCES project_players(id)
        ON DELETE SET NULL NOT VALID;

-- Return-type change forbids CREATE OR REPLACE: recreate the helper so the
-- account portal can render the disabled / deletion-pending state per link.
DROP FUNCTION player_account_linked_projects(uuid);
CREATE FUNCTION public.player_account_linked_projects(p_account_id uuid)
    RETURNS TABLE(player_id bigint, tenant_id bigint, project_id bigint, project_name text, external_id text, linked_at timestamp with time zone, disabled_at timestamp with time zone, delete_requested_at timestamp with time zone)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT e.id, e.tenant_id, e.project_id, p.name::text, e.external_id, e.created_at, e.disabled_at, e.delete_requested_at
    FROM project_players e
    JOIN projects p ON p.id = e.project_id
    WHERE e.player_account_id = p_account_id
      AND e.deleted_at IS NULL
    ORDER BY e.created_at;
$$;

REVOKE ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) TO ggscale_app;
