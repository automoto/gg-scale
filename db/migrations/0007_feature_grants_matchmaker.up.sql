-- Matchmaker is default-on and is turned off by an explicit enabled=false
-- feature_grants row, but the baseline check constraint predates the feature
-- and rejected feature='matchmaker', so a matchmaker deprovision could never
-- be stored.
ALTER TABLE feature_grants DROP CONSTRAINT feature_grants_feature_check;
ALTER TABLE feature_grants ADD CONSTRAINT feature_grants_feature_check
    CHECK (feature = ANY (ARRAY['p2p_relay'::text, 'dedicated_servers'::text, 'fleet_docker_backend'::text, 'fleet_agones_backend'::text, 'fleet_plugin_backend'::text, 'matchmaker'::text]));
