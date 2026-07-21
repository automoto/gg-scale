-- Service actors (e.g. the billing service applying entitlements) write audit
-- rows with no control-panel user. actor_service names the acting service so
-- automated changes stay distinguishable from human approvals.
ALTER TABLE audit_log ADD COLUMN actor_service text;
ALTER TABLE platform_audit_log ADD COLUMN actor_service text;
