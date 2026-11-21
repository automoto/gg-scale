package controlpanel

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/rbac"
)

// allScopes returns every scope of the table.
func allScopes() []string {
	var out []string
	for _, s := range managementtoken.Scopes {
		out = append(out, s.Scope)
	}
	return out
}

// withRiskyScope adds a class 2 scope to the keys row for the test.
func withRiskyScope(t *testing.T) string {
	t.Helper()
	saved := managementtoken.Scopes
	managementtoken.Scopes = append(append([]managementtoken.ScopePair{}, saved...), managementtoken.ScopePair{
		Scope: "keys:risky", Class: managementtoken.ClassRisky, Resource: "keys",
		Pair: func(int64) string { return rbac.ObjectProject }, Action: rbac.ActionManage,
	})
	t.Cleanup(func() { managementtoken.Scopes = saved })
	return "keys:risky"
}

func accessRow(t *testing.T, rows []AccessRowView, name string) AccessRowView {
	t.Helper()
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no access row %q", name)
	return AccessRowView{}
}

func selections(rows []AccessRowView) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		out[r.Name] = r.Selected
	}
	return out
}

func TestAccessOptions_should_disable_write_with_role_reason_when_no_write_scope_is_grantable(t *testing.T) {
	rows, _ := accessOptions([]string{managementtoken.ScopeKeysRead}, nil)

	got := accessRow(t, rows, "keys")

	assert.Equal(t, []any{true, false, reasonRole}, []any{got.CanRead, got.CanWrite, got.Reason})
}

func TestAccessOptions_should_disable_the_row_with_the_gate_reason(t *testing.T) {
	rows, _ := accessOptions(allScopes(), map[string]string{"keys": reasonSwitch})

	got := accessRow(t, rows, "keys")

	assert.Equal(t, []any{false, false, reasonSwitch}, []any{got.CanRead, got.CanWrite, got.Reason})
}

func TestAccessOptions_should_have_no_write_option_for_matchmaking(t *testing.T) {
	rows, _ := accessOptions(allScopes(), nil)

	got := accessRow(t, rows, "matchmaking")

	assert.Equal(t, []any{false, ""}, []any{got.HasWrite, got.Reason})
}

func TestAccessOptions_should_disable_a_risky_box_the_creator_cannot_give(t *testing.T) {
	scope := withRiskyScope(t)

	_, risky := accessOptions(allScopes()[:1], nil)

	assert.Equal(t, []RiskyBoxView{{Scope: scope, Label: scope, Reason: reasonRole}}, risky)
}

func TestScopesFromAccessForm(t *testing.T) {
	rows, risky := accessOptions([]string{
		managementtoken.ScopeConfigRead, managementtoken.ScopeConfigWrite, managementtoken.ScopeOriginsWrite,
		managementtoken.ScopeKeysRead, managementtoken.ScopeMatchmakingRead,
	}, nil)
	cases := []struct {
		name    string
		form    url.Values
		want    []string
		refused bool
	}{
		{name: "should_give_nothing_for_absent_fields", form: url.Values{}, want: []string{}},
		{name: "should_give_read_and_write_scopes_for_write", form: url.Values{"access_config": {"write"}},
			want: []string{managementtoken.ScopeConfigRead, managementtoken.ScopeConfigWrite, managementtoken.ScopeOriginsWrite}},
		{name: "should_give_read_scope_for_read", form: url.Values{"access_keys": {"read"}}, want: []string{managementtoken.ScopeKeysRead}},
		{name: "should_refuse_a_disabled_write", form: url.Values{"access_keys": {"write"}}, refused: true},
		{name: "should_refuse_a_disabled_read", form: url.Values{"access_leaderboards": {"read"}}, refused: true},
		{name: "should_refuse_write_on_a_row_without_write", form: url.Values{"access_matchmaking": {"write"}}, refused: true},
		{name: "should_refuse_an_unknown_value", form: url.Values{"access_config": {"admin"}}, refused: true},
		{name: "should_refuse_a_normal_scope_as_risky", form: url.Values{"risky": {managementtoken.ScopeConfigWrite}}, refused: true},
		{name: "should_refuse_an_unknown_risky_scope", form: url.Values{"risky": {"tenant:admin"}}, refused: true},
		{name: "should_use_only_the_first_value_of_a_duplicate_field", form: url.Values{"access_config": {"read", "write"}},
			want: []string{managementtoken.ScopeConfigRead}},
		{name: "should_give_nothing_for_an_unknown_row_field", form: url.Values{"access_players": {"write"}}, want: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scopesFromAccessForm(tc.form, rows, risky)

			if tc.refused {
				assert.Error(t, err)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestScopesFromAccessForm_should_give_a_checked_risky_scope(t *testing.T) {
	scope := withRiskyScope(t)
	rows, risky := accessOptions(allScopes(), nil)

	got, err := scopesFromAccessForm(url.Values{"risky": {scope}}, rows, risky)
	require.NoError(t, err)

	assert.Equal(t, []string{scope}, got)
}

func TestFillAccess_should_select_read_in_each_row_by_default(t *testing.T) {
	rows, risky := accessOptions(allScopes(), nil)

	fillAccess(url.Values{}, rows, risky)

	assert.Equal(t, map[string]string{"config": "read", "leaderboards": "read", "keys": "read", "matchmaking": "read"}, selections(rows))
}

func TestFillAccess_should_step_down_from_a_disabled_option(t *testing.T) {
	rows, risky := accessOptions([]string{managementtoken.ScopeConfigRead}, nil)

	fillAccess(url.Values{"fill": {"read_write"}}, rows, risky)

	assert.Equal(t, map[string]string{"config": "read", "leaderboards": "none", "keys": "none", "matchmaking": "none"}, selections(rows))
}

func TestFillAccess_read_write_should_write_where_possible(t *testing.T) {
	rows, risky := accessOptions(allScopes(), nil)

	fillAccess(url.Values{"fill": {"read_write"}}, rows, risky)

	assert.Equal(t, map[string]string{"config": "write", "leaderboards": "write", "keys": "write", "matchmaking": "read"}, selections(rows))
}

func TestFillAccess_quick_fill_should_never_check_a_risky_box(t *testing.T) {
	scope := withRiskyScope(t)
	rows, risky := accessOptions(allScopes(), nil)

	fillAccess(url.Values{"fill": {"read_write"}, "risky": {scope}}, rows, risky)

	assert.False(t, risky[0].Checked)
}

func TestFillAccess_should_check_a_risky_box_from_the_link(t *testing.T) {
	scope := withRiskyScope(t)
	rows, risky := accessOptions(allScopes(), nil)

	fillAccess(url.Values{"risky": {scope}}, rows, risky)

	assert.True(t, risky[0].Checked)
}

func TestTokenAccessValues_should_round_trip_through_the_form(t *testing.T) {
	scope := withRiskyScope(t)
	scopes := []string{managementtoken.ScopeConfigRead, managementtoken.ScopeConfigWrite, managementtoken.ScopeOriginsWrite,
		managementtoken.ScopeMatchmakingRead, scope}
	rows, risky := accessOptions(allScopes(), nil)

	got, _ := scopesFromAccessForm(tokenAccessValues(scopes), rows, risky)

	assert.ElementsMatch(t, scopes, got)
}

func TestTokenAccessValues_should_map_a_partial_row_to_read(t *testing.T) {
	got := tokenAccessValues([]string{managementtoken.ScopeConfigRead, managementtoken.ScopeConfigWrite})

	assert.Equal(t, "read", got.Get("access_config"))
}

func TestAccessWords(t *testing.T) {
	cases := []struct {
		name   string
		scopes []string
		want   string
	}{
		{name: "should_say_no_access_for_no_scopes", scopes: nil, want: "No access"},
		{name: "should_name_each_row", scopes: []string{
			managementtoken.ScopeConfigRead, managementtoken.ScopeConfigWrite, managementtoken.ScopeOriginsWrite,
			managementtoken.ScopeLeaderboardsRead,
		}, want: "Remote config and origins: read and write · Leaderboards: read"},
		{name: "should_name_the_extra_scopes_of_a_partial_row", scopes: []string{
			managementtoken.ScopeConfigRead, managementtoken.ScopeConfigWrite,
		}, want: "Remote config and origins: read, config:write"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := accessWords(tc.scopes)

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAccessWords_should_report_risky_access(t *testing.T) {
	scope := withRiskyScope(t)

	words, risky := accessWords([]string{scope})

	assert.Equal(t, []any{"Risky: " + scope, true}, []any{words, risky})
}

func TestFeatureGate(t *testing.T) {
	auth, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	cases := []struct {
		name     string
		switchOn bool
		feature  rbac.Feature
		want     string
	}{
		{name: "should_pass_a_row_without_feature", feature: "", want: ""},
		{name: "should_say_turned_off_when_the_switch_is_off", switchOn: false, feature: rbac.FeatureDedicatedServers, want: reasonSwitch},
		{name: "should_say_not_enabled_when_the_grant_is_missing", switchOn: true, feature: rbac.FeatureDedicatedServers, want: reasonGrant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: Config{FleetEnabled: tc.switchOn}, rbac: auth}

			got, err := h.featureGate(context.Background(), 1, 2, tc.feature)
			require.NoError(t, err)

			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTokenAccessValues_should_fill_keys_write_back_to_write(t *testing.T) {
	rows, risky := accessOptions(allScopes(), nil)
	made, err := scopesFromAccessForm(url.Values{"access_keys": {"write"}}, rows, risky)
	require.NoError(t, err)

	got := tokenAccessValues(made).Get("access_keys")

	assert.ElementsMatch(t, []string{managementtoken.ScopeKeysRead, managementtoken.ScopeKeysCreate, managementtoken.ScopeKeysWrite}, made)
	assert.Equal(t, "write", got)
}

func TestTokenAccessValues_should_map_keys_without_keys_write_to_read(t *testing.T) {
	got := tokenAccessValues([]string{managementtoken.ScopeKeysRead, managementtoken.ScopeKeysCreate})

	assert.Equal(t, "read", got.Get("access_keys"))
}
