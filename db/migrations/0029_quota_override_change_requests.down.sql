DELETE FROM tenant_change_requests WHERE kind = 'quota_override';

ALTER TABLE tenant_change_requests
    DROP CONSTRAINT tenant_change_requests_shape;
ALTER TABLE tenant_change_requests
    ADD CONSTRAINT tenant_change_requests_shape
    CHECK (
        (kind = 'tier_upgrade' AND requested_tier IS NOT NULL AND feature IS NULL) OR
        (kind = 'feature'      AND feature IS NOT NULL AND requested_tier IS NULL)
    );

ALTER TABLE tenant_change_requests
    DROP CONSTRAINT tenant_change_requests_feature_check;
ALTER TABLE tenant_change_requests
    ADD CONSTRAINT tenant_change_requests_feature_check
    CHECK (feature = ANY (ARRAY[
        'p2p_relay', 'dedicated_servers', 'fleet_docker_backend',
        'fleet_agones_backend', 'fleet_plugin_backend', 'matchmaker']));

ALTER TABLE tenant_change_requests
    DROP CONSTRAINT tenant_change_requests_kind_check;
ALTER TABLE tenant_change_requests
    ADD CONSTRAINT tenant_change_requests_kind_check
    CHECK (kind IN ('tier_upgrade', 'feature'));

ALTER TABLE tenant_change_requests
    DROP COLUMN requested_limit;
