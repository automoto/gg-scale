-- Signup / public-join controls (Milestone 3). A player can publicly join a
-- project only when BOTH the tenant master switch AND the per-project toggle
-- allow it: effective policy = tenant.public_joining_enabled AND
-- project.public_joining_enabled. Admin invites bypass both toggles by design.
--
-- Global gg-scale account signup (player_accounts) stays open regardless;
-- these toggles gate LINKING an account into a specific project, not creating
-- the account.

ALTER TABLE tenants
    ADD COLUMN public_joining_enabled BOOLEAN NOT NULL DEFAULT true;

ALTER TABLE projects
    ADD COLUMN public_joining_enabled BOOLEAN NOT NULL DEFAULT true;

-- The player-site public-join flow runs in a global account context (no
-- app.tenant_id), so it cannot read tenants/projects under RLS. This
-- SECURITY DEFINER helper is the single controlled surface: given a project
-- id it returns the tenant id and the EFFECTIVE join policy (tenant AND
-- project). Keyed by project id only; leaks nothing else.
CREATE OR REPLACE FUNCTION project_join_context(p_project_id BIGINT)
RETURNS TABLE (
    tenant_id        BIGINT,
    effective_enabled BOOLEAN,
    project_name     TEXT
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT p.tenant_id,
           (t.public_joining_enabled AND p.public_joining_enabled),
           p.name::text
    FROM projects p
    JOIN tenants t ON t.id = p.tenant_id
    WHERE p.id = p_project_id
      AND p.deleted_at IS NULL
      AND t.deleted_at IS NULL;
$$;

REVOKE ALL ON FUNCTION project_join_context(BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION project_join_context(BIGINT) TO ggscale_app;
