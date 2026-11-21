package controlpanel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/tenant"
)

func TestIsQuotaAxis_should_not_let_tenants_request_leaderboards(t *testing.T) {
	assert.False(t, isQuotaAxis(quota.AxisLeaderboards))
}

func TestIsAdminQuotaAxis_should_let_platform_admins_set_leaderboards(t *testing.T) {
	assert.True(t, isAdminQuotaAxis(quota.AxisLeaderboards))
}

func TestValidateOverrideLimit_should_refuse_unlimited_leaderboards(t *testing.T) {
	assert.ErrorIs(t, validateOverrideLimit(quota.AxisLeaderboards, quota.Unlimited), errLeaderboardsUnlimited)
}

func TestValidateOverrideLimit_should_allow_large_leaderboards_limit(t *testing.T) {
	assert.NoError(t, validateOverrideLimit(quota.AxisLeaderboards, 50000))
}

// quotaRow returns the row of axis from buildQuotaOverrideRows.
func quotaRow(axis string) QuotaOverrideRowView {
	for _, r := range buildQuotaOverrideRows(tenant.Tier0, nil) {
		if r.Axis == axis {
			return r
		}
	}
	return QuotaOverrideRowView{}
}

func TestBuildQuotaOverrideRows_should_list_leaderboards_with_configured_default(t *testing.T) {
	quota.SetLeaderboardsPerProject(25)
	t.Cleanup(func() { quota.SetLeaderboardsPerProject(1000) })

	got := quotaRow(quota.AxisLeaderboards).DefaultLabel

	assert.Equal(t, "25", got)
}

func TestBuildQuotaOverrideRows_should_not_offer_unlimited_leaderboards(t *testing.T) {
	got := quotaRow(quota.AxisLeaderboards).Min

	assert.Equal(t, "0", got)
}

func TestBuildQuotaOverrideRows_should_offer_unlimited_for_other_axes(t *testing.T) {
	got := quotaRow(quota.AxisPlayers).Min

	assert.Equal(t, "-1", got)
}

func TestBuildQuotaOverrideRows_should_not_offer_unlimited_open_sessions(t *testing.T) {
	row := quotaRow(quota.AxisOpenSessions)

	assert.Equal(t, []any{"0", true}, []any{row.Min, strings.Contains(row.Note, "cannot be unlimited")})
}

func TestBuildQuotaOverrideRows_should_say_the_leaderboards_limit_always_applies(t *testing.T) {
	got := quotaRow(quota.AxisLeaderboards).Note

	assert.Contains(t, got, "always applies")
}

func TestBuildQuotaOverrideRows_should_say_other_limits_apply_with_enforcement_on(t *testing.T) {
	got := quotaRow(quota.AxisPlayers).Note

	assert.Contains(t, got, "quota enforcement is on")
}

func TestRateLimitsPage_should_show_the_leaderboards_row_note_and_min(t *testing.T) {
	html := renderToString(t, RateLimitsPage(RateLimitsView{
		TenantID:        7,
		IsPlatformAdmin: true,
		QuotaOverrides:  []QuotaOverrideRowView{quotaRow(quota.AxisLeaderboards)},
	}))

	assert.Equal(t, []bool{true, true, false},
		[]bool{strings.Contains(html, "always applies"), strings.Contains(html, `min="0"`), strings.Contains(html, `min="-1"`)})
}
