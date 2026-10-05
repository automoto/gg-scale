-- A deletion that a project admin requested cannot be cancelled by the player.
-- Only an admin cancel clears it.
ALTER TABLE project_players
    ADD COLUMN delete_requested_by_admin boolean NOT NULL DEFAULT false;

-- The account portal hides the cancel action for admin requests.
DROP FUNCTION player_account_linked_projects(uuid);
CREATE FUNCTION public.player_account_linked_projects(p_account_id uuid)
    RETURNS TABLE(player_id bigint, tenant_id bigint, project_id bigint, project_name text, external_id text, linked_at timestamp with time zone, disabled_at timestamp with time zone, delete_requested_at timestamp with time zone, delete_requested_by_admin boolean)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT e.id, e.tenant_id, e.project_id, p.name::text, e.external_id, e.created_at, e.disabled_at, e.delete_requested_at, e.delete_requested_by_admin
    FROM project_players e
    JOIN projects p ON p.id = e.project_id
    WHERE e.player_account_id = p_account_id
      AND e.deleted_at IS NULL
    ORDER BY e.created_at;
$$;

REVOKE ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) TO ggscale_app;
