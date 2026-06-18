GRANT SELECT, INSERT, DELETE ON casbin_rule TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE casbin_rule_id_seq TO ggscale_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON feature_grants TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE feature_grants_id_seq TO ggscale_app;
