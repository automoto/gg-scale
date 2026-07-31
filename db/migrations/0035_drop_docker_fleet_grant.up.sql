-- Drop the Docker fleet backend feature. Its allocator was removed because it
-- was not safe for testing or production; fleet features now ship only via the
-- Agones and plugin backends. Remove any lingering grants/requests that name it,
-- then narrow both feature CHECK constraints so the token can no longer be
-- stored.
DELETE FROM feature_grants WHERE feature = 'fleet_docker_backend';
DELETE FROM tenant_change_requests WHERE feature = 'fleet_docker_backend';

ALTER TABLE feature_grants DROP CONSTRAINT feature_grants_feature_check;
ALTER TABLE feature_grants ADD CONSTRAINT feature_grants_feature_check
    CHECK (feature = ANY (ARRAY['p2p_relay'::text, 'dedicated_servers'::text, 'fleet_agones_backend'::text, 'fleet_plugin_backend'::text, 'matchmaker'::text]));

ALTER TABLE tenant_change_requests DROP CONSTRAINT tenant_change_requests_feature_check;
ALTER TABLE tenant_change_requests ADD CONSTRAINT tenant_change_requests_feature_check
    CHECK (
        (kind <> 'quota_override' AND feature = ANY (ARRAY[
            'p2p_relay', 'dedicated_servers',
            'fleet_agones_backend', 'fleet_plugin_backend', 'matchmaker']))
        OR
        (kind = 'quota_override' AND feature = ANY (ARRAY[
            'projects', 'players', 'storage', 'relay_sessions', 'open_sessions']))
    );
