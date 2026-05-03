-- Dashboard bootstrap flow: an operator creates a tenant, its first project,
-- and the first API key before any tenant-scoped request context exists.
-- SECURITY DEFINER lets the narrow function insert through RLS while keeping
-- normal dashboard key management tenant-scoped via db.Q.

CREATE OR REPLACE FUNCTION dashboard_create_tenant(
    p_tenant_name TEXT,
    p_project_name TEXT,
    p_key_hash BYTEA,
    p_key_label TEXT
)
RETURNS TABLE (
    tenant_id BIGINT,
    project_id BIGINT,
    api_key_id BIGINT
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public
AS $$
DECLARE
    v_label TEXT := nullif(trim(p_key_label), '');
BEGIN
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

    INSERT INTO audit_log (tenant_id, action, target, payload)
    VALUES (
        tenant_id,
        'dashboard.signup',
        'tenant:' || tenant_id::TEXT,
        jsonb_build_object('project_id', project_id, 'api_key_id', api_key_id)
    );

    RETURN NEXT;
END;
$$;

REVOKE ALL ON FUNCTION dashboard_create_tenant(TEXT, TEXT, BYTEA, TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION dashboard_create_tenant(TEXT, TEXT, BYTEA, TEXT) TO ggscale_app;
