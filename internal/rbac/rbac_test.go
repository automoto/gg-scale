package rbac_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

func newAuthorizer(t *testing.T) *rbac.Authorizer {
	t.Helper()
	a, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	return a
}

func TestDefaultPolicy_allows_tenant_admin_in_own_domain(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "admin"))

	allowed, err := a.CanControlPanel(rbac.ControlPanelUser{
		ID: 42,
	}, 7, rbac.ObjectProject, rbac.ActionManage)

	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDefaultPolicy_denies_tenant_admin_in_other_domain(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "admin"))

	allowed, err := a.CanControlPanel(rbac.ControlPanelUser{
		ID: 42,
	}, 8, rbac.ObjectProject, rbac.ActionManage)

	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestDefaultPolicy_treats_current_member_as_read_only_analyst(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "member"))

	allowed, err := a.CanControlPanel(rbac.ControlPanelUser{
		ID: 42,
	}, 7, rbac.ObjectProject, rbac.ActionManage)

	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestDefaultPolicy_glob_matches_colon_delimited_project_objects(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "admin"))

	allowed, err := a.CanControlPanel(rbac.ControlPanelUser{
		ID: 42,
	}, 7, rbac.ProjectPlayersObject(99), rbac.ActionManage)

	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDefaultPolicy_allows_tenant_owner_to_allocate_project_fleet(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "owner"))

	allowed, err := a.CanControlPanel(rbac.ControlPanelUser{
		ID: 42,
	}, 7, rbac.ProjectAllocationObject(99), rbac.ActionAllocate)

	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDefaultPolicy_allows_tenant_admin_and_owner_to_manage_leaderboards(t *testing.T) {
	for _, role := range []string{"admin", "owner"} {
		t.Run(role, func(t *testing.T) {
			a := newAuthorizer(t)
			require.NoError(t, a.SetControlPanelMembershipRole(42, 7, role))

			allowed, err := a.CanControlPanel(rbac.ControlPanelUser{
				ID: 42,
			}, 7, rbac.ProjectLeaderboardObject(99), rbac.ActionManage)

			require.NoError(t, err)
			assert.True(t, allowed)
		})
	}
}

func TestDefaultPolicy_denies_member_leaderboard_management(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "member"))

	allowed, err := a.CanControlPanel(rbac.ControlPanelUser{
		ID: 42,
	}, 7, rbac.ProjectLeaderboardObject(99), rbac.ActionManage)

	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestPlatformAdmin_manages_any_tenant_control_panel(t *testing.T) {
	a := newAuthorizer(t)
	// Platform admin with no membership in tenant 7.
	pa := rbac.ControlPanelUser{ID: 99, IsPlatformAdmin: true}

	cases := []struct {
		name     string
		obj, act string
	}{
		{"project manage", rbac.ObjectProject, rbac.ActionManage},
		{"tenant manage", rbac.ObjectTenant, rbac.ActionManage},
		{"players manage", rbac.ProjectPlayersObject(99), rbac.ActionManage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed, err := a.CanControlPanel(pa, 7, tc.obj, tc.act)
			require.NoError(t, err)
			assert.True(t, allowed)
		})
	}
}

func TestNonPlatformAdmin_without_membership_denied(t *testing.T) {
	a := newAuthorizer(t)
	allowed, err := a.CanControlPanel(rbac.ControlPanelUser{ID: 99, IsPlatformAdmin: false},
		7, rbac.ObjectProject, rbac.ActionManage)
	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestTenantOwner_manages_both_api_key_types(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "owner"))

	for _, obj := range []string{rbac.ObjectAPIKeySecret, rbac.ObjectAPIKeyPublic} {
		allowed, err := a.CanControlPanel(rbac.ControlPanelUser{ID: 42}, 7, obj, rbac.ActionManage)
		require.NoError(t, err)
		assert.True(t, allowed, "owner manages %s", obj)
	}
}

func TestTenantAdmin_manages_only_publishable_api_keys(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "admin"))

	pub, err := a.CanControlPanel(rbac.ControlPanelUser{ID: 42}, 7, rbac.ObjectAPIKeyPublic, rbac.ActionManage)
	require.NoError(t, err)
	assert.True(t, pub, "admin manages publishable keys")

	sec, err := a.CanControlPanel(rbac.ControlPanelUser{ID: 42}, 7, rbac.ObjectAPIKeySecret, rbac.ActionManage)
	require.NoError(t, err)
	assert.False(t, sec, "admin must not create/manage secret keys")
}

func TestTeamManagement_is_owner_only(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(1, 7, "owner"))
	require.NoError(t, a.SetControlPanelMembershipRole(2, 7, "admin"))

	ownerAllowed, err := a.CanControlPanel(rbac.ControlPanelUser{ID: 1}, 7, rbac.ObjectTeam, rbac.ActionManage)
	require.NoError(t, err)
	assert.True(t, ownerAllowed, "owner manages the team")

	adminAllowed, err := a.CanControlPanel(rbac.ControlPanelUser{ID: 2}, 7, rbac.ObjectTeam, rbac.ActionManage)
	require.NoError(t, err)
	assert.False(t, adminAllowed, "team management (invites, role grants, removals) is owner-only")
}

func TestDefaultPolicy_api_key_roles_preserve_secret_boundaries(t *testing.T) {
	a := newAuthorizer(t)

	publishable, err := a.CanAPIKey(tenant.APIKey{
		ID: 1, TenantID: 7, Type: tenant.KeyTypePublishable,
	}, rbac.ObjectLeaderboard, rbac.ActionSubmit)
	require.NoError(t, err)
	assert.False(t, publishable)

	secret, err := a.CanAPIKey(tenant.APIKey{
		ID: 2, TenantID: 7, Type: tenant.KeyTypeSecret,
	}, rbac.ObjectLeaderboard, rbac.ActionSubmit)
	require.NoError(t, err)
	assert.True(t, secret)
}

func TestDefaultPolicy_api_key_explicit_role_is_tenant_scoped(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.AddAPIKeyRole(2, 7, tenant.KeyTypeSecret))

	allowed, err := a.CanAPIKey(tenant.APIKey{
		ID: 2, TenantID: 8, Type: tenant.KeyTypePublishable,
	}, rbac.ObjectLeaderboard, rbac.ActionSubmit)

	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestDefaultPolicy_allows_relay_and_dedicated_matchmaking_for_standard_player(t *testing.T) {
	a := newAuthorizer(t)

	relay, err := a.CanPlayer(7, 123, rbac.ProjectRelayObject(99), rbac.ActionIssueCredentials)
	require.NoError(t, err)
	assert.True(t, relay, "relay credential issuance is a base player capability, gated by feature_grants + key scope")

	dedicated, err := a.CanPlayer(7, 123, rbac.ProjectDedicatedMatchmakingObject(99), rbac.ActionCreateTicket)
	require.NoError(t, err)
	assert.True(t, dedicated)
}

func TestFleetOperator_coexists_with_membership_role(t *testing.T) {
	a := newAuthorizer(t)
	// A member with the analyst membership role, then also granted fleet_operator.
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "member"))
	require.NoError(t, a.AddControlPanelRole(42, 7, rbac.RoleFleetOperator))

	u := rbac.ControlPanelUser{ID: 42}
	// fleet_operator capability now applies...
	manage, err := a.CanControlPanel(u, 7, rbac.ProjectFleetObject(99), rbac.ActionManage)
	require.NoError(t, err)
	assert.True(t, manage, "fleet_operator grants fleet manage")
	// ...and the analyst membership capability is still intact.
	read, err := a.CanControlPanel(u, 7, rbac.ObjectProject, rbac.ActionRead)
	require.NoError(t, err)
	assert.True(t, read, "membership role survives the extra grant")

	has, err := a.HasControlPanelRole(42, 7, rbac.RoleFleetOperator)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestFleetOperator_revoke_leaves_membership(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetControlPanelMembershipRole(42, 7, "member"))
	require.NoError(t, a.AddControlPanelRole(42, 7, rbac.RoleFleetOperator))
	require.NoError(t, a.RemoveControlPanelRole(42, 7, rbac.RoleFleetOperator))

	u := rbac.ControlPanelUser{ID: 42}
	manage, err := a.CanControlPanel(u, 7, rbac.ProjectFleetObject(99), rbac.ActionManage)
	require.NoError(t, err)
	assert.False(t, manage, "fleet manage gone after revoke")
	read, err := a.CanControlPanel(u, 7, rbac.ObjectProject, rbac.ActionRead)
	require.NoError(t, err)
	assert.True(t, read, "membership role untouched by revoke")
}

func TestFeatureEnabled_denies_by_default(t *testing.T) {
	// A fresh authorizer with no feature_grants backing store must deny every
	// high-risk feature — the entitlement layer is deny-by-default.
	a := newAuthorizer(t)
	for _, f := range []rbac.Feature{rbac.FeatureP2PRelay, rbac.FeatureDedicatedServers, rbac.FeatureFleetDockerBackend} {
		enabled, err := a.FeatureEnabled(t.Context(), 7, 99, f)
		require.NoError(t, err)
		assert.False(t, enabled, "feature %q must be off until a feature_grants row enables it", f)
	}
}

func TestAddControlPanelRole_rejects_non_grantable_role(t *testing.T) {
	a := newAuthorizer(t)
	err := a.AddControlPanelRole(42, 7, rbac.RoleTenantOwner)
	assert.Error(t, err, "membership roles are not à-la-carte grantable")
}

func TestAddPlayerRole_rejects_non_player_role(t *testing.T) {
	a := newAuthorizer(t)

	err := a.AddPlayerRole(123, 7, rbac.RolePlatformAdmin)

	assert.Error(t, err)
}

func TestRoleMutations_return_error_when_authorizer_unavailable(t *testing.T) {
	var a *rbac.Authorizer

	err := a.AddPlayerRole(123, 7, rbac.RolePlayerVerified)

	assert.True(t, errors.Is(err, rbac.ErrAuthorizerUnavailable))
}

func TestBackendFeature_maps_backend_names(t *testing.T) {
	cases := []struct {
		backend string
		want    rbac.Feature
		ok      bool
	}{
		{"docker", rbac.FeatureFleetDockerBackend, true},
		{"agones", rbac.FeatureFleetAgonesBackend, true},
		{"plugin:ovh", rbac.FeatureFleetPluginBackend, true},
		{"memory", "", false},
	}
	for _, c := range cases {
		t.Run(c.backend, func(t *testing.T) {
			got, ok := rbac.BackendFeature(c.backend)
			assert.Equal(t, c.ok, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestFeatureEnabled_matchmaker_defaults_on(t *testing.T) {
	// Matchmaker is not a high-risk feature: it works with zero config on a
	// fresh install. Only an explicit enabled=false feature_grants row (or a
	// DB-backed disable) turns it off.
	a := newAuthorizer(t)
	enabled, err := a.FeatureEnabled(t.Context(), 7, 99, rbac.FeatureMatchmaker)
	require.NoError(t, err)
	assert.True(t, enabled, "matchmaker must be enabled with no feature_grants row")
}
