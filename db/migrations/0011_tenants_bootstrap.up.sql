-- The tenant middleware resolves an api_key + JOIN tenants to fetch the
-- tier in one round trip. Without this permissive bootstrap SELECT
-- policy, the JOIN's tenants side returns zero rows when the
-- app.tenant_id GUC is unset (which it always is at the auth handshake).
-- Mirrors api_keys_bootstrap from migration 0010.

CREATE POLICY tenants_bootstrap ON tenants
    FOR SELECT
    USING (
        current_setting('app.tenant_id', true) IS NULL
        OR current_setting('app.tenant_id', true) = ''
    );
