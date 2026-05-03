DROP FUNCTION IF EXISTS dashboard_create_tenant(TEXT, TEXT, BYTEA, TEXT);

CREATE OR REPLACE FUNCTION dashboard_create_tenant(
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

    INSERT INTO tenants (name)
    VALUES (trim(p_tenant_name))
    RETURNING id INTO tenant_id;

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
