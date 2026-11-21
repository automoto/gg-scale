-- Remove leaderboards overrides, then narrow the axis CHECK again. Removed
-- rows are not recoverable; projects fall back to LEADERBOARD_MAX_PER_PROJECT.
DELETE FROM tenant_quota_overrides WHERE axis = 'leaderboards';
ALTER TABLE tenant_quota_overrides DROP CONSTRAINT tenant_quota_overrides_axis_check;
ALTER TABLE tenant_quota_overrides ADD CONSTRAINT tenant_quota_overrides_axis_check
    CHECK (axis IN ('projects', 'players', 'storage', 'relay_sessions', 'open_sessions', 'relay_allocations'));
