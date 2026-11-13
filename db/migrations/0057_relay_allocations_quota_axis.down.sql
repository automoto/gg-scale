-- Remove relay_allocations overrides, then narrow the axis CHECK again. Removed
-- rows are not recoverable; relays fall back to the class value.
DELETE FROM tenant_quota_overrides WHERE axis = 'relay_allocations';
ALTER TABLE tenant_quota_overrides DROP CONSTRAINT tenant_quota_overrides_axis_check;
ALTER TABLE tenant_quota_overrides ADD CONSTRAINT tenant_quota_overrides_axis_check
    CHECK (axis IN ('projects', 'players', 'storage', 'relay_sessions', 'open_sessions'));
