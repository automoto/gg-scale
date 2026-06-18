package rbac_test

import (
	"context"
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
	require.NoError(t, a.SetDashboardMembershipRole(42, 7, "admin"))

	allowed, err := a.CanDashboard(context.Background(), rbac.DashboardUser{
		ID: 42,
	}, 7, rbac.ObjectProject, rbac.ActionManage)

	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDefaultPolicy_denies_tenant_admin_in_other_domain(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetDashboardMembershipRole(42, 7, "admin"))

	allowed, err := a.CanDashboard(context.Background(), rbac.DashboardUser{
		ID: 42,
	}, 8, rbac.ObjectProject, rbac.ActionManage)

	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestDefaultPolicy_treats_current_member_as_read_only_analyst(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetDashboardMembershipRole(42, 7, "member"))

	allowed, err := a.CanDashboard(context.Background(), rbac.DashboardUser{
		ID: 42,
	}, 7, rbac.ObjectProject, rbac.ActionManage)

	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestDefaultPolicy_glob_matches_colon_delimited_project_objects(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.SetDashboardMembershipRole(42, 7, "admin"))

	allowed, err := a.CanDashboard(context.Background(), rbac.DashboardUser{
		ID: 42,
	}, 7, rbac.ProjectPlayersObject(99), rbac.ActionManage)

	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestDefaultPolicy_api_key_roles_preserve_secret_boundaries(t *testing.T) {
	a := newAuthorizer(t)

	publishable, err := a.CanAPIKey(context.Background(), tenant.APIKey{
		ID: 1, TenantID: 7, Type: tenant.KeyTypePublishable,
	}, rbac.ObjectLeaderboard, rbac.ActionSubmit)
	require.NoError(t, err)
	assert.False(t, publishable)

	secret, err := a.CanAPIKey(context.Background(), tenant.APIKey{
		ID: 2, TenantID: 7, Type: tenant.KeyTypeSecret,
	}, rbac.ObjectLeaderboard, rbac.ActionSubmit)
	require.NoError(t, err)
	assert.True(t, secret)
}

func TestDefaultPolicy_denies_high_risk_player_access_by_default(t *testing.T) {
	a := newAuthorizer(t)

	allowed, err := a.CanEndUser(context.Background(), 7, 99, 123, rbac.ProjectRelayObject(99), rbac.ActionIssueCredentials)

	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestDefaultPolicy_allows_explicit_high_access_player_role(t *testing.T) {
	a := newAuthorizer(t)
	require.NoError(t, a.AddEndUserRole(123, 7, rbac.RolePlayerHighAccess))

	allowed, err := a.CanEndUser(context.Background(), 7, 99, 123, rbac.ProjectRelayObject(99), rbac.ActionIssueCredentials)

	require.NoError(t, err)
	assert.True(t, allowed)
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
