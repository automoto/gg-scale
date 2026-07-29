-- Extend tenant change requests with a quota_override kind: the feature
-- column carries the quota axis (internal/quota Axis* constants) and the new
-- requested_limit column carries the asked-for value (-1 = unlimited).
-- Approval upserts a tenant_quota_overrides row.
ALTER TABLE tenant_change_requests
    ADD COLUMN requested_limit BIGINT CHECK (requested_limit >= -1);

ALTER TABLE tenant_change_requests
    DROP CONSTRAINT tenant_change_requests_kind_check;
ALTER TABLE tenant_change_requests
    ADD CONSTRAINT tenant_change_requests_kind_check
    CHECK (kind IN ('tier_upgrade', 'feature', 'quota_override'));

ALTER TABLE tenant_change_requests
    DROP CONSTRAINT tenant_change_requests_feature_check;
ALTER TABLE tenant_change_requests
    ADD CONSTRAINT tenant_change_requests_feature_check
    CHECK (
        (kind <> 'quota_override' AND feature = ANY (ARRAY[
            'p2p_relay', 'dedicated_servers', 'fleet_docker_backend',
            'fleet_agones_backend', 'fleet_plugin_backend', 'matchmaker']))
        OR
        (kind = 'quota_override' AND feature = ANY (ARRAY[
            'projects', 'players', 'storage', 'relay_sessions', 'open_sessions']))
    );

ALTER TABLE tenant_change_requests
    DROP CONSTRAINT tenant_change_requests_shape;
ALTER TABLE tenant_change_requests
    ADD CONSTRAINT tenant_change_requests_shape
    CHECK (
        (kind = 'tier_upgrade'   AND requested_tier IS NOT NULL AND feature IS NULL AND requested_limit IS NULL) OR
        (kind = 'feature'        AND feature IS NOT NULL AND requested_tier IS NULL AND requested_limit IS NULL) OR
        (kind = 'quota_override' AND feature IS NOT NULL AND requested_limit IS NOT NULL AND requested_tier IS NULL)
    );
