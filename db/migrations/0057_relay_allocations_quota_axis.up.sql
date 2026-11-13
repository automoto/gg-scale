-- Allow the relay_allocations axis: a platform-admin override of the tenant's
-- live TURN allocation cap on each relay node (quota.AxisRelayAllocations).
-- Expand only. Releases before this one ignore override axes they do not know
-- (quota.Resolve and the control-panel quota rows skip them).
--
-- tenant_change_requests_feature_check deliberately stays without this axis:
-- tenants cannot request it; only a platform admin sets it directly.
ALTER TABLE tenant_quota_overrides DROP CONSTRAINT tenant_quota_overrides_axis_check;
ALTER TABLE tenant_quota_overrides ADD CONSTRAINT tenant_quota_overrides_axis_check
    CHECK (axis IN ('projects', 'players', 'storage', 'relay_sessions', 'open_sessions', 'relay_allocations'));
