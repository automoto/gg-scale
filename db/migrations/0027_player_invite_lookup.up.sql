-- Player-facing invite acceptance: the player UI lands on a magic-link
-- URL whose only context is the project_id from the path. RLS on
-- projects, end_user_invitations, and end_users requires app.tenant_id
-- to be set first, but we cannot set it until we know the tenant — and
-- we discover the tenant from the invite row itself.
--
-- This SECURITY DEFINER helper does the privileged lookup (bypassing
-- RLS) so the caller can SET app.tenant_id and proceed with the rest
-- of the flow under normal RLS enforcement. The function is tightly
-- scoped: it returns only the fields the player UI needs and exposes
-- no other rows.

CREATE OR REPLACE FUNCTION player_invite_lookup(p_code_hash BYTEA)
RETURNS TABLE (
    id          BIGINT,
    tenant_id   BIGINT,
    project_id  BIGINT,
    email       CITEXT,
    expires_at  TIMESTAMPTZ,
    project_name TEXT
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT
        i.id,
        i.tenant_id,
        i.project_id,
        i.email,
        i.expires_at,
        p.name
    FROM end_user_invitations i
    JOIN projects p ON p.id = i.project_id
    WHERE i.code_hash = p_code_hash
      AND i.accepted_at IS NULL
      AND i.revoked_at IS NULL;
$$;

REVOKE ALL ON FUNCTION player_invite_lookup(BYTEA) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION player_invite_lookup(BYTEA) TO ggscale_app;

-- Companion to player_invite_lookup: the session-issuing path needs to
-- resolve end_user.tenant_id WITHOUT app.tenant_id already set (the
-- player UI knows the user but not yet the tenant during signup /
-- invite acceptance). SECURITY DEFINER returns nothing but the tenant
-- so it's narrowly scoped.

CREATE OR REPLACE FUNCTION player_end_user_tenant(p_end_user_id BIGINT)
RETURNS TABLE (
    tenant_id BIGINT
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT tenant_id
    FROM end_users
    WHERE id = p_end_user_id
      AND deleted_at IS NULL;
$$;

REVOKE ALL ON FUNCTION player_end_user_tenant(BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION player_end_user_tenant(BIGINT) TO ggscale_app;
