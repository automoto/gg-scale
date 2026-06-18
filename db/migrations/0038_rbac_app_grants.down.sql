REVOKE USAGE, SELECT ON SEQUENCE feature_grants_id_seq FROM ggscale_app;
REVOKE SELECT, INSERT, UPDATE, DELETE ON feature_grants FROM ggscale_app;

REVOKE USAGE, SELECT ON SEQUENCE casbin_rule_id_seq FROM ggscale_app;
REVOKE SELECT, INSERT, DELETE ON casbin_rule FROM ggscale_app;
