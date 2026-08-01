-- Custom tokens move from a shared HMAC secret to public-key verification:
-- the developer's backend signs with a private key (Ed25519 or RSA) and
-- ggscale stores only the public key, so the database holds nothing that can
-- mint player sessions. The old symmetric secret column is dropped outright
-- (pre-1.0 deliberate break; existing HS256 integrations must re-key).

-- Surface the break to whoever applies this migration: every tenant counted
-- here loses custom-token sign-in until an operator saves a public key in
-- Tenant settings (docs/custom-token-auth.md has the tenant-facing guide).
DO $$
DECLARE affected bigint;
BEGIN
    SELECT count(*) INTO affected FROM tenants
    WHERE custom_token_secret IS NOT NULL AND length(custom_token_secret) > 0;
    IF affected > 0 THEN
        RAISE WARNING 'custom-token: dropping the HS256 secret of % tenant(s); '
            'their custom-token sign-in is disabled until a public key is configured '
            '(see docs/custom-token-auth.md)', affected;
    END IF;
END $$;

ALTER TABLE tenants
ADD COLUMN custom_token_public_key text NOT NULL DEFAULT '',
DROP COLUMN custom_token_secret;

-- Tenant owners and admins manage their own signing key in the control panel
-- (security_admin already holds custom_token manage).
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
VALUES
    ('p', 'role:platform_admin', '*', 'custom_token', 'manage'),
    ('p', 'role:tenant_owner', '*', 'custom_token', 'manage'),
    ('p', 'role:tenant_admin', '*', 'custom_token', 'manage')
ON CONFLICT DO NOTHING;
