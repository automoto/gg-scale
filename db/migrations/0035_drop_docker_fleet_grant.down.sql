-- Restore fleet_docker_backend as an accepted feature token in both CHECK
-- constraints. Rows deleted by the up migration are not recoverable.
ALTER TABLE feature_grants DROP CONSTRAINT feature_grants_feature_check;
ALTER TABLE feature_grants ADD CONSTRAINT feature_grants_feature_check
    CHECK (feature = ANY (ARRAY['p2p_relay'::text, 'dedicated_servers'::text, 'fleet_docker_backend'::text, 'fleet_agones_backend'::text, 'fleet_plugin_backend'::text, 'matchmaker'::text]));

ALTER TABLE tenant_change_requests DROP CONSTRAINT tenant_change_requests_feature_check;
ALTER TABLE tenant_change_requests ADD CONSTRAINT tenant_change_requests_feature_check
    CHECK (
        (kind <> 'quota_override' AND feature = ANY (ARRAY[
            'p2p_relay', 'dedicated_servers', 'fleet_docker_backend',
            'fleet_agones_backend', 'fleet_plugin_backend', 'matchmaker']))
        OR
        (kind = 'quota_override' AND feature = ANY (ARRAY[
            'projects', 'players', 'storage', 'relay_sessions', 'open_sessions']))
    );
