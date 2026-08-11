DROP FUNCTION player_account_linked_projects(uuid);
CREATE FUNCTION public.player_account_linked_projects(p_account_id uuid)
    RETURNS TABLE(player_id bigint, tenant_id bigint, project_id bigint, project_name text, external_id text, linked_at timestamp with time zone)
    LANGUAGE sql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
    SELECT e.id, e.tenant_id, e.project_id, p.name::text, e.external_id, e.created_at
    FROM project_players e
    JOIN projects p ON p.id = e.project_id
    WHERE e.player_account_id = p_account_id
      AND e.deleted_at IS NULL
    ORDER BY e.created_at;
$$;

REVOKE ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) FROM PUBLIC;
GRANT ALL ON FUNCTION public.player_account_linked_projects(p_account_id uuid) TO ggscale_app;

ALTER TABLE audit_log
    DROP CONSTRAINT audit_log_actor_user_id_fkey;
ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_actor_user_id_fkey
        FOREIGN KEY (actor_user_id) REFERENCES project_players(id) NOT VALID;

DROP INDEX project_players_delete_requested_idx;
ALTER TABLE project_players
    DROP COLUMN delete_requested_at;
