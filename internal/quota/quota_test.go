package quota_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/quota"
	"github.com/ggscale/ggscale/internal/tenant"
)

func TestLimitsForClass_ladder_values(t *testing.T) {
	const gb = int64(1) << 30
	cases := []struct {
		tier     tenant.Tier
		projects int
		players  int64
		storage  int64
	}{
		{tenant.Tier0, 3, 250_000, 5 * gb},
		{tenant.Tier1, 10, 1_000_000, 25 * gb},
		{tenant.Tier2, 20, 5_000_000, 100 * gb},
		{tenant.Tier3, quota.Unlimited, quota.Unlimited, 500 * gb},
	}
	for _, tc := range cases {
		got := quota.LimitsForClass(tc.tier)
		assert.Equal(t, tc.projects, got.Projects, "tier=%s projects", tc.tier)
		assert.Equal(t, tc.players, got.Players, "tier=%s players", tc.tier)
		assert.Equal(t, tc.storage, got.StorageBytes, "tier=%s storage", tc.tier)
	}
}

func TestLimitsForClass_unknown_class_falls_back_to_tier0(t *testing.T) {
	assert.Equal(t, quota.LimitsForClass(tenant.Tier0), quota.LimitsForClass(tenant.Tier(99)))
}

func TestCheckProjects_allows_below_limit(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier0) // 3
	assert.NoError(t, l.CheckProjects(0))
	assert.NoError(t, l.CheckProjects(2))
}

func TestCheckProjects_rejects_at_and_above_limit(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier0) // 3
	err := l.CheckProjects(3)

	var qe *quota.ErrQuotaExceeded
	assert.ErrorAs(t, err, &qe)
	assert.Equal(t, quota.AxisProjects, qe.Axis)
	assert.Equal(t, int64(3), qe.Limit)
	assert.Equal(t, int64(3), qe.Current)
}

func TestCheckPlayers_rejects_at_limit(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier0) // 250k
	err := l.CheckPlayers(250_000)

	var qe *quota.ErrQuotaExceeded
	assert.ErrorAs(t, err, &qe)
	assert.Equal(t, quota.AxisPlayers, qe.Axis)
}

func TestCheck_unlimited_never_rejects(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier3) // unlimited projects + players
	assert.NoError(t, l.CheckProjects(1_000_000))
	assert.NoError(t, l.CheckPlayers(1_000_000_000))
}

func TestCheckStorage_blocks_growing_write_over_limit(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier0) // 5 GB
	const gb = int64(1) << 30

	// At exactly the limit a further growing write is rejected.
	err := l.CheckStorage(5*gb, 1)
	var qe *quota.ErrQuotaExceeded
	assert.ErrorAs(t, err, &qe)
	assert.Equal(t, quota.AxisStorage, qe.Axis)
}

func TestCheckStorage_allows_shrinking_and_within_limit(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier0) // 5 GB
	const gb = int64(1) << 30

	assert.NoError(t, l.CheckStorage(5*gb, -100), "shrink always allowed")
	assert.NoError(t, l.CheckStorage(4*gb, 100), "growth within limit allowed")
	assert.NoError(t, l.CheckStorage(0, 0), "no-op allowed")
}

func TestErrQuotaExceeded_is_a_distinct_error(t *testing.T) {
	err := (&quota.ErrQuotaExceeded{Axis: quota.AxisProjects, Limit: 3, Current: 3})
	assert.True(t, errors.As(error(err), new(*quota.ErrQuotaExceeded)))
	assert.Contains(t, err.Error(), "projects")
}
