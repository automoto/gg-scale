-- Optional public tenant sign-up with manual platform-admin approval.
-- Access is deny-by-default: a developer submits a request, a platform admin
-- approves or denies it, and the tenant is created only when an approved
-- requester accepts the emailed invite (control_panel_create_tenant_bare).
-- Platform-global tables (no RLS, explicit filtering), read via BootstrapQ.

CREATE TABLE tenant_signup_requests (
    id                    BIGSERIAL PRIMARY KEY,
    email                 public.citext NOT NULL,
    requested_tenant_name TEXT NOT NULL,
    -- final_tenant_name is what the admin approves (they may correct the
    -- requested name); it's the name the tenant is created with on accept.
    final_tenant_name     TEXT,
    project_description   TEXT NOT NULL,
    studio_name           TEXT,
    status                TEXT NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'approved', 'denied', 'accepted')),
    -- Invite code (hash only) minted at approval; reuses the verifycode convention.
    code_hash             BYTEA,
    code_expires_at       TIMESTAMPTZ,
    reviewed_by_user_id   BIGINT REFERENCES control_panel_users(id) ON DELETE SET NULL,
    reviewed_at           TIMESTAMPTZ,
    review_reason         TEXT,
    tenant_id             BIGINT REFERENCES tenants(id) ON DELETE SET NULL,
    accepted_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One request per email, forever: enforces "one self-signup-owned tenant per
-- email" and makes a denial permanent (the denied row keeps the email's slot,
-- so the same address can't re-apply).
CREATE UNIQUE INDEX tenant_signup_requests_email_key
    ON tenant_signup_requests (email);

-- No two live (pending or approved) requests may claim the same tenant name.
-- Denied/accepted rows are excluded so a freed name can be reused elsewhere.
CREATE UNIQUE INDEX tenant_signup_requests_live_name_key
    ON tenant_signup_requests (lower(COALESCE(final_tenant_name, requested_tenant_name)))
    WHERE status IN ('pending', 'approved');

-- Lookup by hashed invite code during acceptance.
CREATE INDEX tenant_signup_requests_code_hash_idx
    ON tenant_signup_requests (code_hash)
    WHERE code_hash IS NOT NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_signup_requests TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE tenant_signup_requests_id_seq TO ggscale_app;

-- Platform-global toggle for the public sign-up form. Single row (id = 1),
-- defaults OFF: access is never granted by default.
CREATE TABLE platform_signup_config (
    id                           SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    public_tenant_signup_enabled BOOLEAN NOT NULL DEFAULT false,
    updated_by                   BIGINT REFERENCES control_panel_users(id) ON DELETE SET NULL,
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO platform_signup_config (id, public_tenant_signup_enabled)
VALUES (1, false)
ON CONFLICT (id) DO NOTHING;

GRANT SELECT, UPDATE ON platform_signup_config TO ggscale_app;

-- Enforce tenant name uniqueness going forward (case-insensitive, ignoring
-- soft-deleted tenants so a deleted tenant's name can be reused).
CREATE UNIQUE INDEX tenants_name_unique_idx
    ON tenants (lower(name))
    WHERE deleted_at IS NULL;

-- Tenant-only creation path used by public-signup acceptance: creates the
-- tenant + owner membership + audit row, with NO project and NO api key (the
-- new owner sets those up after onboarding). Mirrors control_panel_create_tenant.
CREATE FUNCTION public.control_panel_create_tenant_bare(p_actor_user_id bigint, p_tenant_name text)
    RETURNS bigint
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'public'
    AS $$
DECLARE
    v_tenant_id bigint;
    v_membership_id bigint;
BEGIN
    IF p_actor_user_id IS NULL OR p_actor_user_id <= 0 THEN
        RAISE EXCEPTION 'control panel actor user id is required' USING ERRCODE = '22023';
    END IF;
    IF nullif(trim(p_tenant_name), '') IS NULL THEN
        RAISE EXCEPTION 'tenant name is required' USING ERRCODE = '22023';
    END IF;

    PERFORM set_config('app.allow_tenant_bootstrap', '1', true);

    INSERT INTO tenants (name)
    VALUES (trim(p_tenant_name))
    RETURNING id INTO v_tenant_id;

    PERFORM set_config('app.tenant_id', v_tenant_id::TEXT, true);

    INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role)
    VALUES (p_actor_user_id, v_tenant_id, 'owner')
    RETURNING id INTO v_membership_id;

    INSERT INTO audit_log (tenant_id, action, target, payload)
    VALUES (
        v_tenant_id,
        'control_panel.tenant.created',
        'tenant:' || v_tenant_id::TEXT,
        jsonb_build_object(
            'control_panel_user_id', p_actor_user_id,
            'membership_id', v_membership_id,
            'via', 'public_signup'
        )
    );

    RETURN v_tenant_id;
END;
$$;

REVOKE ALL ON FUNCTION public.control_panel_create_tenant_bare(bigint, text) FROM PUBLIC;
GRANT ALL ON FUNCTION public.control_panel_create_tenant_bare(bigint, text) TO ggscale_app;
