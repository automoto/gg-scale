-- Reverse the control-panel rename: control_panel_* → dashboard_*.

-- Stored RBAC (Casbin) rows.
UPDATE casbin_rule SET v2 = 'dashboard_user' WHERE v2 = 'control_panel_user';
UPDATE casbin_rule SET v0 = replace(v0, 'control_panel:user:', 'dashboard:user:')
    WHERE v0 LIKE 'control_panel:user:%';

-- Foreign-key constraints.
ALTER TABLE feature_grants RENAME CONSTRAINT feature_grants_approved_by_control_panel_user_id_fkey TO feature_grants_approved_by_dashboard_user_id_fkey;
ALTER TABLE control_panel_trusted_devices RENAME CONSTRAINT control_panel_trusted_devices_user_id_fkey TO dashboard_trusted_devices_dashboard_user_id_fkey;
ALTER TABLE control_panel_user_totp_backup_codes RENAME CONSTRAINT control_panel_user_totp_backup_codes_user_id_fkey TO dashboard_user_totp_backup_codes_dashboard_user_id_fkey;
ALTER TABLE control_panel_user_totp   RENAME CONSTRAINT control_panel_user_totp_user_id_fkey    TO dashboard_user_totp_dashboard_user_id_fkey;
ALTER TABLE control_panel_invitations RENAME CONSTRAINT control_panel_invitations_tenant_id_fkey          TO dashboard_invitations_tenant_id_fkey;
ALTER TABLE control_panel_invitations RENAME CONSTRAINT control_panel_invitations_invited_by_user_id_fkey TO dashboard_invitations_invited_by_user_id_fkey;
ALTER TABLE control_panel_sessions    RENAME CONSTRAINT control_panel_sessions_user_id_fkey    TO dashboard_sessions_dashboard_user_id_fkey;
ALTER TABLE control_panel_memberships RENAME CONSTRAINT control_panel_memberships_tenant_id_fkey         TO dashboard_memberships_tenant_id_fkey;
ALTER TABLE control_panel_memberships RENAME CONSTRAINT control_panel_memberships_user_id_fkey TO dashboard_memberships_dashboard_user_id_fkey;

-- Check constraints.
ALTER TABLE control_panel_invitations RENAME CONSTRAINT control_panel_invitations_role_check TO dashboard_invitations_role_check;
ALTER TABLE control_panel_memberships RENAME CONSTRAINT control_panel_memberships_role_check TO dashboard_memberships_role_check;

-- Unique constraints.
ALTER TABLE control_panel_user_totp_backup_codes RENAME CONSTRAINT control_panel_user_totp_backup_codes_user_id_code_hash_key TO dashboard_user_totp_backup_code_dashboard_user_id_code_hash_key;
ALTER TABLE control_panel_memberships      RENAME CONSTRAINT control_panel_memberships_user_id_tenant_id_key TO dashboard_memberships_dashboard_user_id_tenant_id_key;
ALTER TABLE control_panel_trusted_devices  RENAME CONSTRAINT control_panel_trusted_devices_token_hash_key TO dashboard_trusted_devices_token_hash_key;
ALTER TABLE control_panel_sessions         RENAME CONSTRAINT control_panel_sessions_refresh_hash_key  TO dashboard_sessions_refresh_hash_key;
ALTER TABLE control_panel_users            RENAME CONSTRAINT control_panel_users_email_key            TO dashboard_users_email_key;

-- Secondary indexes.
ALTER INDEX control_panel_invitations_code_lookup_idx    RENAME TO dashboard_invitations_code_lookup_idx;
ALTER INDEX control_panel_invitations_tenant_idx         RENAME TO dashboard_invitations_tenant_idx;
ALTER INDEX control_panel_invitations_open_uq            RENAME TO dashboard_invitations_open_uq;
ALTER INDEX control_panel_trusted_devices_user_idx       RENAME TO dashboard_trusted_devices_user_idx;
ALTER INDEX control_panel_sessions_user_active_idx       RENAME TO dashboard_sessions_user_active_idx;
ALTER INDEX control_panel_memberships_tenant_idx         RENAME TO dashboard_memberships_tenant_idx;
ALTER INDEX control_panel_users_disabled_idx             RENAME TO dashboard_users_disabled_idx;
ALTER INDEX control_panel_users_created_id_idx            RENAME TO dashboard_users_created_id_idx;
ALTER INDEX control_panel_users_email_trgm_idx            RENAME TO dashboard_users_email_trgm_idx;

-- Primary keys.
ALTER INDEX control_panel_trusted_devices_pkey          RENAME TO dashboard_trusted_devices_pkey;
ALTER INDEX control_panel_user_totp_backup_codes_pkey   RENAME TO dashboard_user_totp_backup_codes_pkey;
ALTER INDEX control_panel_user_totp_pkey                RENAME TO dashboard_user_totp_pkey;
ALTER INDEX control_panel_invitations_pkey              RENAME TO dashboard_invitations_pkey;
ALTER INDEX control_panel_sessions_pkey                 RENAME TO dashboard_sessions_pkey;
ALTER INDEX control_panel_memberships_pkey              RENAME TO dashboard_memberships_pkey;
ALTER INDEX control_panel_users_pkey                    RENAME TO dashboard_users_pkey;

-- Sequences.
ALTER SEQUENCE control_panel_user_totp_backup_codes_id_seq RENAME TO dashboard_user_totp_backup_codes_id_seq;
ALTER SEQUENCE control_panel_trusted_devices_id_seq        RENAME TO dashboard_trusted_devices_id_seq;
ALTER SEQUENCE control_panel_invitations_id_seq            RENAME TO dashboard_invitations_id_seq;
ALTER SEQUENCE control_panel_sessions_id_seq               RENAME TO dashboard_sessions_id_seq;
ALTER SEQUENCE control_panel_memberships_id_seq            RENAME TO dashboard_memberships_id_seq;
ALTER SEQUENCE control_panel_users_id_seq                  RENAME TO dashboard_users_id_seq;

-- Columns.
ALTER TABLE feature_grants RENAME COLUMN approved_by_control_panel_user_id TO approved_by_dashboard_user_id;
ALTER TABLE control_panel_trusted_devices        RENAME COLUMN control_panel_user_id TO dashboard_user_id;
ALTER TABLE control_panel_user_totp_backup_codes RENAME COLUMN control_panel_user_id TO dashboard_user_id;
ALTER TABLE control_panel_user_totp              RENAME COLUMN control_panel_user_id TO dashboard_user_id;
ALTER TABLE control_panel_sessions               RENAME COLUMN control_panel_user_id TO dashboard_user_id;
ALTER TABLE control_panel_memberships            RENAME COLUMN control_panel_user_id TO dashboard_user_id;

-- Tables.
ALTER TABLE control_panel_trusted_devices        RENAME TO dashboard_trusted_devices;
ALTER TABLE control_panel_user_totp_backup_codes RENAME TO dashboard_user_totp_backup_codes;
ALTER TABLE control_panel_user_totp              RENAME TO dashboard_user_totp;
ALTER TABLE control_panel_invitations            RENAME TO dashboard_invitations;
ALTER TABLE control_panel_sessions               RENAME TO dashboard_sessions;
ALTER TABLE control_panel_memberships            RENAME TO dashboard_memberships;
ALTER TABLE control_panel_users                  RENAME TO dashboard_users;

-- Tenant-membership RLS policy (reads app.dashboard_user_id again).
DROP POLICY tenants_control_panel_membership ON tenants;
CREATE POLICY tenants_dashboard_membership ON tenants
    FOR SELECT
    USING (
        nullif(current_setting('app.dashboard_user_id', true), '') IS NOT NULL
        AND EXISTS (
            SELECT 1
            FROM dashboard_users u
            WHERE u.id = nullif(current_setting('app.dashboard_user_id', true), '')::bigint
              AND (
                  u.is_platform_admin
                  OR EXISTS (
                      SELECT 1
                      FROM dashboard_memberships m
                      WHERE m.dashboard_user_id = u.id
                        AND m.tenant_id = tenants.id
                  )
              )
        )
    );

-- Tenant-bootstrap helper (original body).
DROP FUNCTION control_panel_create_tenant(BIGINT, TEXT, TEXT, BYTEA, TEXT);

CREATE FUNCTION dashboard_create_tenant(
    p_actor_user_id BIGINT,
    p_tenant_name TEXT,
    p_project_name TEXT,
    p_key_hash BYTEA,
    p_key_label TEXT
)
RETURNS TABLE (
    tenant_id BIGINT,
    project_id BIGINT,
    api_key_id BIGINT,
    membership_id BIGINT
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_label TEXT := nullif(trim(p_key_label), '');
BEGIN
    IF p_actor_user_id IS NULL OR p_actor_user_id <= 0 THEN
        RAISE EXCEPTION 'dashboard actor user id is required' USING ERRCODE = '22023';
    END IF;
    IF nullif(trim(p_tenant_name), '') IS NULL THEN
        RAISE EXCEPTION 'tenant name is required' USING ERRCODE = '22023';
    END IF;
    IF nullif(trim(p_project_name), '') IS NULL THEN
        RAISE EXCEPTION 'project name is required' USING ERRCODE = '22023';
    END IF;
    IF p_key_hash IS NULL OR length(p_key_hash) = 0 THEN
        RAISE EXCEPTION 'api key hash is required' USING ERRCODE = '22023';
    END IF;

    PERFORM set_config('app.allow_tenant_bootstrap', '1', true);

    INSERT INTO tenants (name)
    VALUES (trim(p_tenant_name))
    RETURNING id INTO tenant_id;

    PERFORM set_config('app.tenant_id', tenant_id::TEXT, true);

    INSERT INTO projects (tenant_id, name)
    VALUES (tenant_id, trim(p_project_name))
    RETURNING id INTO project_id;

    INSERT INTO api_keys (tenant_id, project_id, key_hash, label, scopes)
    VALUES (tenant_id, project_id, p_key_hash, v_label, '{}'::TEXT[])
    RETURNING id INTO api_key_id;

    INSERT INTO dashboard_memberships (dashboard_user_id, tenant_id, role)
    VALUES (p_actor_user_id, tenant_id, 'owner')
    RETURNING id INTO membership_id;

    INSERT INTO audit_log (tenant_id, action, target, payload)
    VALUES (
        tenant_id,
        'dashboard.tenant.created',
        'tenant:' || tenant_id::TEXT,
        jsonb_build_object(
            'dashboard_user_id', p_actor_user_id,
            'project_id', project_id,
            'api_key_id', api_key_id,
            'membership_id', membership_id
        )
    );

    RETURN NEXT;
END;
$$;

REVOKE ALL ON FUNCTION dashboard_create_tenant(BIGINT, TEXT, TEXT, BYTEA, TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION dashboard_create_tenant(BIGINT, TEXT, TEXT, BYTEA, TEXT) TO ggscale_app;
