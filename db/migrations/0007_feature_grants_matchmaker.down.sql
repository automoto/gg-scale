-- Restoring the narrower constraint requires dropping any matchmaker rows;
-- affected tenants revert to the feature's default (enabled).
DELETE FROM feature_grants WHERE feature = 'matchmaker';
ALTER TABLE feature_grants DROP CONSTRAINT feature_grants_feature_check;
ALTER TABLE feature_grants ADD CONSTRAINT feature_grants_feature_check
    CHECK (feature = ANY (ARRAY['p2p_relay'::text, 'dedicated_servers'::text, 'fleet_docker_backend'::text, 'fleet_agones_backend'::text, 'fleet_plugin_backend'::text]));
