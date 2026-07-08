-- Rename the operator web UI from "dashboard" to "control panel".
--
-- The admin surface (operators who log in to manage tenants, projects, and
-- keys) is the control panel; the ambiguous "dashboard" token — which also
-- reads as a player-facing view — is retired everywhere. Hard cut, matching
-- the end_user→player rename precedent (0060): no compatibility shims. No live
-- data outside the seeded RBAC rows, so ALTER … RENAME is transactional and
-- lossless. Renames cover tables, columns, sequences, indexes, constraints,
-- the tenant-membership RLS policy (which also reads a renamed session GUC),
-- the tenant-bootstrap SECURITY DEFINER helper, and the stored Casbin rows.

-- Tables.
ALTER TABLE dashboard_users                  RENAME TO control_panel_users;
ALTER TABLE dashboard_memberships            RENAME TO control_panel_memberships;
ALTER TABLE dashboard_sessions               RENAME TO control_panel_sessions;
ALTER TABLE dashboard_invitations            RENAME TO control_panel_invitations;
ALTER TABLE dashboard_user_totp              RENAME TO control_panel_user_totp;
ALTER TABLE dashboard_user_totp_backup_codes RENAME TO control_panel_user_totp_backup_codes;
ALTER TABLE dashboard_trusted_devices        RENAME TO control_panel_trusted_devices;

-- Columns.
ALTER TABLE control_panel_memberships            RENAME COLUMN dashboard_user_id TO control_panel_user_id;
ALTER TABLE control_panel_sessions               RENAME COLUMN dashboard_user_id TO control_panel_user_id;
ALTER TABLE control_panel_user_totp              RENAME COLUMN dashboard_user_id TO control_panel_user_id;
ALTER TABLE control_panel_user_totp_backup_codes RENAME COLUMN dashboard_user_id TO control_panel_user_id;
ALTER TABLE control_panel_trusted_devices        RENAME COLUMN dashboard_user_id TO control_panel_user_id;
ALTER TABLE feature_grants RENAME COLUMN approved_by_dashboard_user_id TO approved_by_control_panel_user_id;

-- Sequences.
ALTER SEQUENCE dashboard_users_id_seq                  RENAME TO control_panel_users_id_seq;
ALTER SEQUENCE dashboard_memberships_id_seq            RENAME TO control_panel_memberships_id_seq;
ALTER SEQUENCE dashboard_sessions_id_seq               RENAME TO control_panel_sessions_id_seq;
ALTER SEQUENCE dashboard_invitations_id_seq            RENAME TO control_panel_invitations_id_seq;
ALTER SEQUENCE dashboard_trusted_devices_id_seq        RENAME TO control_panel_trusted_devices_id_seq;
ALTER SEQUENCE dashboard_user_totp_backup_codes_id_seq RENAME TO control_panel_user_totp_backup_codes_id_seq;

-- Primary keys (renaming the backing index renames the constraint too).
ALTER INDEX dashboard_users_pkey                    RENAME TO control_panel_users_pkey;
ALTER INDEX dashboard_memberships_pkey              RENAME TO control_panel_memberships_pkey;
ALTER INDEX dashboard_sessions_pkey                 RENAME TO control_panel_sessions_pkey;
ALTER INDEX dashboard_invitations_pkey              RENAME TO control_panel_invitations_pkey;
ALTER INDEX dashboard_user_totp_pkey                RENAME TO control_panel_user_totp_pkey;
ALTER INDEX dashboard_user_totp_backup_codes_pkey   RENAME TO control_panel_user_totp_backup_codes_pkey;
ALTER INDEX dashboard_trusted_devices_pkey          RENAME TO control_panel_trusted_devices_pkey;

-- Secondary indexes.
ALTER INDEX dashboard_users_email_trgm_idx            RENAME TO control_panel_users_email_trgm_idx;
ALTER INDEX dashboard_users_created_id_idx            RENAME TO control_panel_users_created_id_idx;
ALTER INDEX dashboard_users_disabled_idx             RENAME TO control_panel_users_disabled_idx;
ALTER INDEX dashboard_memberships_tenant_idx         RENAME TO control_panel_memberships_tenant_idx;
ALTER INDEX dashboard_sessions_user_active_idx       RENAME TO control_panel_sessions_user_active_idx;
ALTER INDEX dashboard_trusted_devices_user_idx       RENAME TO control_panel_trusted_devices_user_idx;
ALTER INDEX dashboard_invitations_open_uq            RENAME TO control_panel_invitations_open_uq;
ALTER INDEX dashboard_invitations_tenant_idx         RENAME TO control_panel_invitations_tenant_idx;
ALTER INDEX dashboard_invitations_code_lookup_idx    RENAME TO control_panel_invitations_code_lookup_idx;

-- Unique constraints.
ALTER TABLE control_panel_users            RENAME CONSTRAINT dashboard_users_email_key            TO control_panel_users_email_key;
ALTER TABLE control_panel_sessions         RENAME CONSTRAINT dashboard_sessions_refresh_hash_key  TO control_panel_sessions_refresh_hash_key;
ALTER TABLE control_panel_trusted_devices  RENAME CONSTRAINT dashboard_trusted_devices_token_hash_key TO control_panel_trusted_devices_token_hash_key;
ALTER TABLE control_panel_memberships      RENAME CONSTRAINT dashboard_memberships_dashboard_user_id_tenant_id_key TO control_panel_memberships_user_id_tenant_id_key;
ALTER TABLE control_panel_user_totp_backup_codes RENAME CONSTRAINT dashboard_user_totp_backup_code_dashboard_user_id_code_hash_key TO control_panel_user_totp_backup_codes_user_id_code_hash_key;

-- Check constraints.
ALTER TABLE control_panel_memberships RENAME CONSTRAINT dashboard_memberships_role_check TO control_panel_memberships_role_check;
ALTER TABLE control_panel_invitations RENAME CONSTRAINT dashboard_invitations_role_check TO control_panel_invitations_role_check;

-- Foreign-key constraints.
ALTER TABLE control_panel_memberships RENAME CONSTRAINT dashboard_memberships_dashboard_user_id_fkey TO control_panel_memberships_user_id_fkey;
ALTER TABLE control_panel_memberships RENAME CONSTRAINT dashboard_memberships_tenant_id_fkey         TO control_panel_memberships_tenant_id_fkey;
ALTER TABLE control_panel_sessions    RENAME CONSTRAINT dashboard_sessions_dashboard_user_id_fkey    TO control_panel_sessions_user_id_fkey;
ALTER TABLE control_panel_invitations RENAME CONSTRAINT dashboard_invitations_invited_by_user_id_fkey TO control_panel_invitations_invited_by_user_id_fkey;
ALTER TABLE control_panel_invitations RENAME CONSTRAINT dashboard_invitations_tenant_id_fkey          TO control_panel_invitations_tenant_id_fkey;
ALTER TABLE control_panel_user_totp   RENAME CONSTRAINT dashboard_user_totp_dashboard_user_id_fkey    TO control_panel_user_totp_user_id_fkey;
ALTER TABLE control_panel_user_totp_backup_codes RENAME CONSTRAINT dashboard_user_totp_backup_codes_dashboard_user_id_fkey TO control_panel_user_totp_backup_codes_user_id_fkey;
ALTER TABLE control_panel_trusted_devices RENAME CONSTRAINT dashboard_trusted_devices_dashboard_user_id_fkey TO control_panel_trusted_devices_user_id_fkey;
ALTER TABLE feature_grants RENAME CONSTRAINT feature_grants_approved_by_dashboard_user_id_fkey TO feature_grants_approved_by_control_panel_user_id_fkey;

-- Tenant-membership RLS policy: recreated (not renamed) because its body reads
-- the renamed session GUC app.control_panel_user_id, which ALTER POLICY cannot
-- change.
DROP POLICY tenants_dashboard_membership ON tenants;
CREATE POLICY tenants_control_panel_membership ON tenants
    FOR SELECT
    USING (
        nullif(current_setting('app.control_panel_user_id', true), '') IS NOT NULL
        AND EXISTS (
            SELECT 1
            FROM control_panel_users u
            WHERE u.id = nullif(current_setting('app.control_panel_user_id', true), '')::bigint
              AND (
                  u.is_platform_admin
                  OR EXISTS (
                      SELECT 1
                      FROM control_panel_memberships m
                      WHERE m.control_panel_user_id = u.id
                        AND m.tenant_id = tenants.id
                  )
              )
        )
    );

-- Tenant-bootstrap helper: renamed with its body updated for the new table,
-- column, audit-action, and payload-key names.
DROP FUNCTION dashboard_create_tenant(BIGINT, TEXT, TEXT, BYTEA, TEXT);

CREATE FUNCTION control_panel_create_tenant(
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
        RAISE EXCEPTION 'control panel actor user id is required' USING ERRCODE = '22023';
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

    INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role)
    VALUES (p_actor_user_id, tenant_id, 'owner')
    RETURNING id INTO membership_id;

    INSERT INTO audit_log (tenant_id, action, target, payload)
    VALUES (
        tenant_id,
        'control_panel.tenant.created',
        'tenant:' || tenant_id::TEXT,
        jsonb_build_object(
            'control_panel_user_id', p_actor_user_id,
            'project_id', project_id,
            'api_key_id', api_key_id,
            'membership_id', membership_id
        )
    );

    RETURN NEXT;
END;
$$;

REVOKE ALL ON FUNCTION control_panel_create_tenant(BIGINT, TEXT, TEXT, BYTEA, TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION control_panel_create_tenant(BIGINT, TEXT, TEXT, BYTEA, TEXT) TO ggscale_app;

-- Stored RBAC (Casbin) rows: the operator object and per-operator subject
-- prefix. Historical audit_log action strings keep their old values by design;
-- only these live authorization rows move.
UPDATE casbin_rule SET v2 = 'control_panel_user' WHERE v2 = 'dashboard_user';
UPDATE casbin_rule SET v0 = replace(v0, 'dashboard:user:', 'control_panel:user:')
    WHERE v0 LIKE 'dashboard:user:%';
