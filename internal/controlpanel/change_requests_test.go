package controlpanel

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/gamesession"
)

func TestParseRequestedTier(t *testing.T) {
	cases := []struct {
		in      string
		want    int16
		wantErr bool
	}{
		{"1", 1, false},
		{"3", 3, false},
		{"0", 0, false},
		{"4", 0, true},
		{"-1", 0, true},
		{"", 0, true},
		{"two", 0, true},
	}
	for _, tc := range cases {
		got, err := parseRequestedTier(tc.in)
		if tc.wantErr {
			assert.Error(t, err, "in=%q", tc.in)
			continue
		}
		assert.NoError(t, err, "in=%q", tc.in)
		assert.Equal(t, tc.want, got, "in=%q", tc.in)
	}
}

func TestParseRequestedLimit(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"500", 500, false},
		{" 2500 ", 2500, false},
		{"-1", -1, false},
		{"-2", 0, true},
		{"", 0, true},
		{"lots", 0, true},
	}
	for _, tc := range cases {
		got, err := parseRequestedLimit(tc.in)
		if tc.wantErr {
			assert.Error(t, err, "in=%q", tc.in)
			continue
		}
		assert.NoError(t, err, "in=%q", tc.in)
		assert.Equal(t, tc.want, got, "in=%q", tc.in)
	}
}

func TestValidateOverrideLimit_open_sessions_bounded_by_hard_cap(t *testing.T) {
	assert.NoError(t, validateOverrideLimit("open_sessions", 0))
	assert.NoError(t, validateOverrideLimit("open_sessions", gamesession.SessionsHardCap))
	assert.Error(t, validateOverrideLimit("open_sessions", gamesession.SessionsHardCap+1),
		"an override above the hard cap promises capacity creation will never grant")
	assert.Error(t, validateOverrideLimit("open_sessions", -1),
		"unlimited is impossible under the absolute hard cap")
}

func TestValidateOverrideLimit_other_axes_allow_unlimited(t *testing.T) {
	assert.NoError(t, validateOverrideLimit("players", -1))
	assert.NoError(t, validateOverrideLimit("relay_sessions", 0))
	assert.NoError(t, validateOverrideLimit("storage", 1<<40))
}

func TestIsQuotaAxis(t *testing.T) {
	assert.True(t, isQuotaAxis("open_sessions"))
	assert.True(t, isQuotaAxis("players"))
	assert.False(t, isQuotaAxis("p2p_relay"), "features are not quota axes")
	assert.False(t, isQuotaAxis(""))
}

func TestQuotaOverrideDetail(t *testing.T) {
	axis := "open_sessions"
	limit := int64(9000)
	unlimited := int64(-1)

	assert.Equal(t, "Open game sessions per project → 9000", quotaOverrideDetail(&axis, &limit))
	assert.Equal(t, "Open game sessions per project → unlimited", quotaOverrideDetail(&axis, &unlimited))
	assert.Equal(t, "", quotaOverrideDetail(nil, &limit), "missing axis renders empty")
	assert.Equal(t, "", quotaOverrideDetail(&axis, nil), "missing limit renders empty")
}

func TestTierIsUpgrade(t *testing.T) {
	cases := []struct {
		name      string
		requested int16
		current   int16
		want      bool
	}{
		{"above current is an upgrade", 2, 1, true},
		{"top from bottom is an upgrade", 3, 0, true},
		{"same class is not an upgrade", 2, 2, false},
		{"below current is not an upgrade", 0, 2, false},
		{"out-of-range current clamps to tier_0", 1, 99, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, tierIsUpgrade(tc.requested, tc.current), tc.name)
	}
}

func TestFeatureEnabledByEnv_respects_kill_switches(t *testing.T) {
	on := &Handler{cfg: Config{FleetEnabled: true, RelayEnabled: true}}
	off := &Handler{cfg: Config{FleetEnabled: false, RelayEnabled: false}}

	assert.True(t, on.featureEnabledByEnv("p2p_relay"))
	assert.True(t, on.featureEnabledByEnv("dedicated_servers"))
	assert.False(t, off.featureEnabledByEnv("p2p_relay"))
	assert.False(t, off.featureEnabledByEnv("dedicated_servers"))
	assert.False(t, on.featureEnabledByEnv("matchmaker"), "matchmaker is not a requestable feature")
}

func TestIsRequestableFeature_only_env_enabled_known_features(t *testing.T) {
	on := &Handler{cfg: Config{FleetEnabled: true, RelayEnabled: true}}
	assert.True(t, on.isRequestableFeature("dedicated_servers"))
	assert.False(t, on.isRequestableFeature("unknown_feature"))

	relayOff := &Handler{cfg: Config{FleetEnabled: true, RelayEnabled: false}}
	assert.False(t, relayOff.isRequestableFeature("p2p_relay"))
}

func TestChangeRequestsPage_lists_pending_requests(t *testing.T) {
	html := renderToString(t, ChangeRequestsPage(ChangeRequestsView{
		Requests: []PendingChangeRequestView{
			{ID: 1, TenantID: 7, TenantName: "acme", CurrentTier: "tier_0", Kind: "tier_upgrade", Target: "tier_2", Note: "launch soon", CreatedAt: time.Unix(0, 0)},
			{ID: 2, TenantID: 8, TenantName: "globex", CurrentTier: "tier_1", Kind: "feature", Target: "p2p_relay", CreatedAt: time.Unix(0, 0)},
		},
	}))
	assert.Contains(t, html, "acme")
	assert.Contains(t, html, "tier_0")
	assert.Contains(t, html, "tier_2")
	assert.Contains(t, html, "p2p_relay")
	assert.Contains(t, html, "/change-requests/1/approve")
	assert.Contains(t, html, "/change-requests/2/deny")
}

func TestChangeRequestsPage_empty_state(t *testing.T) {
	html := renderToString(t, ChangeRequestsPage(ChangeRequestsView{}))
	assert.Contains(t, html, "No pending requests")
}

func TestTenantSettingsPage_renders_denied_reason(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, TenantName: "acme", Tier: "tier_0",
		CanRequestUpgrade: true,
		UpgradeTargets:    []FeatureOptionView{{Value: "1", Label: "tier_1"}},
		ChangeRequests: []ChangeRequestView{
			{Kind: "feature", Detail: "p2p_relay", Status: "denied", ReviewReason: "not on this plan"},
		},
	}))
	assert.Contains(t, html, "Request tier upgrade")
	assert.Contains(t, html, "not on this plan")
	assert.True(t, strings.Contains(html, "settings/change-requests"), "form posts to the submit route")
}
