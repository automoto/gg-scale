package controlpanel

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
