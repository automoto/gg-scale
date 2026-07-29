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
		tier          tenant.Tier
		projects      int
		players       int64
		storage       int64
		relaySessions int64
		openSessions  int64
	}{
		{tenant.Tier0, 3, 100_000, 5 * gb, 1_000, 500},
		{tenant.Tier1, 10, 500_000, 25 * gb, 10_000, 2_000},
		{tenant.Tier2, 20, 2_000_000, 100 * gb, 100_000, 5_000},
		{tenant.Tier3, quota.Unlimited, quota.Unlimited, 500 * gb, quota.Unlimited, 10_000},
	}
	for _, tc := range cases {
		got := quota.LimitsForClass(tc.tier)
		assert.Equal(t, tc.projects, got.Projects, "tier=%s projects", tc.tier)
		assert.Equal(t, tc.players, got.Players, "tier=%s players", tc.tier)
		assert.Equal(t, tc.storage, got.StorageBytes, "tier=%s storage", tc.tier)
		assert.Equal(t, tc.relaySessions, got.RelaySessionsPerMonth, "tier=%s relay sessions", tc.tier)
		assert.Equal(t, tc.openSessions, got.OpenSessionsPerProject, "tier=%s open sessions", tc.tier)
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
	l := quota.LimitsForClass(tenant.Tier0) // 100k
	err := l.CheckPlayers(100_000)

	var qe *quota.ErrQuotaExceeded
	assert.ErrorAs(t, err, &qe)
	assert.Equal(t, quota.AxisPlayers, qe.Axis)
}

func TestCheck_unlimited_never_rejects(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier3) // unlimited projects + players
	assert.NoError(t, l.CheckProjects(1_000_000))
	assert.NoError(t, l.CheckPlayers(1_000_000_000))
}

func TestCheckOpenSessions_allows_below_limit(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier0) // 500
	assert.NoError(t, l.CheckOpenSessions(0))
	assert.NoError(t, l.CheckOpenSessions(499))
}

func TestCheckOpenSessions_rejects_at_limit(t *testing.T) {
	l := quota.LimitsForClass(tenant.Tier0) // 500
	err := l.CheckOpenSessions(500)

	var qe *quota.ErrQuotaExceeded
	assert.ErrorAs(t, err, &qe)
	assert.Equal(t, quota.AxisOpenSessions, qe.Axis)
	assert.Equal(t, int64(500), qe.Limit)
	assert.Equal(t, int64(500), qe.Current)
}

func TestCheckOpenSessions_unlimited_never_rejects(t *testing.T) {
	l := quota.Limits{OpenSessionsPerProject: quota.Unlimited}
	assert.NoError(t, l.CheckOpenSessions(1_000_000))
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

func TestResolve_no_overrides_returns_ladder(t *testing.T) {
	assert.Equal(t, quota.LimitsForClass(tenant.Tier1), quota.Resolve(tenant.Tier1, nil))
}

func TestResolve_applies_overrides_per_axis(t *testing.T) {
	got := quota.Resolve(tenant.Tier0, map[string]int64{
		quota.AxisProjects:      7,
		quota.AxisPlayers:       1_000,
		quota.AxisStorage:       42,
		quota.AxisRelaySessions: quota.Unlimited,
		quota.AxisOpenSessions:  9_000,
	})

	assert.Equal(t, 7, got.Projects)
	assert.Equal(t, int64(1_000), got.Players)
	assert.Equal(t, int64(42), got.StorageBytes)
	assert.Equal(t, int64(quota.Unlimited), got.RelaySessionsPerMonth)
	assert.Equal(t, int64(9_000), got.OpenSessionsPerProject)
}

func TestResolve_ignores_unknown_axis(t *testing.T) {
	got := quota.Resolve(tenant.Tier0, map[string]int64{"bogus": 1})
	assert.Equal(t, quota.LimitsForClass(tenant.Tier0), got)
}

func TestResolveSnapshot_combines_clamp_parse_resolve(t *testing.T) {
	got, err := quota.ResolveSnapshot(1, []byte(`{"open_sessions": 3000}`))
	assert.NoError(t, err)
	want := quota.LimitsForClass(tenant.Tier1)
	want.OpenSessionsPerProject = 3_000
	assert.Equal(t, want, got)
}

func TestResolveSnapshot_rejects_malformed_overrides(t *testing.T) {
	_, err := quota.ResolveSnapshot(0, []byte(`{`))
	assert.Error(t, err)
}

func TestParseOverrides_decodes_jsonb_object(t *testing.T) {
	got, err := quota.ParseOverrides([]byte(`{"players": 250000, "open_sessions": -1}`))
	assert.NoError(t, err)
	assert.Equal(t, map[string]int64{"players": 250_000, "open_sessions": -1}, got)
}

func TestParseOverrides_empty_input_yields_nil(t *testing.T) {
	got, err := quota.ParseOverrides(nil)
	assert.NoError(t, err)
	assert.Nil(t, got)
}

func TestParseOverrides_rejects_malformed_json(t *testing.T) {
	_, err := quota.ParseOverrides([]byte(`not json`))
	assert.Error(t, err)
}

func TestErrQuotaExceeded_is_a_distinct_error(t *testing.T) {
	err := (&quota.ErrQuotaExceeded{Axis: quota.AxisProjects, Limit: 3, Current: 3})
	assert.True(t, errors.As(error(err), new(*quota.ErrQuotaExceeded)))
	assert.Contains(t, err.Error(), "projects")
}
