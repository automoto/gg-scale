package dashboard

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

func TestApplyScope_grant_and_revoke(t *testing.T) {
	cases := []struct {
		name   string
		start  []string
		scope  string
		grant  bool
		expect []string
	}{
		{"grant to empty", nil, "fleet", true, []string{"fleet"}},
		{"grant idempotent", []string{"fleet"}, "fleet", true, []string{"fleet"}},
		{"grant second", []string{"fleet"}, "p2p_relay", true, []string{"fleet", "p2p_relay"}},
		{"revoke present", []string{"fleet", "p2p_relay"}, "fleet", false, []string{"p2p_relay"}},
		{"revoke absent", []string{"p2p_relay"}, "fleet", false, []string{"p2p_relay"}},
		{"revoke last", []string{"fleet"}, "fleet", false, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.expect, applyScope(c.start, c.scope, c.grant))
		})
	}
}

func TestScopeGrantable_denies_by_default(t *testing.T) {
	auth, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)

	// Env switch on, but no feature_grants row (memory authorizer) → not grantable.
	h := &Handler{cfg: Config{FleetEnabled: true, RelayEnabled: true}, rbac: auth}
	assert.False(t, h.scopeGrantable(t.Context(), 7, nil, tenant.ScopeFleet),
		"no feature_grants row → fleet scope not grantable")
	assert.False(t, h.scopeGrantable(t.Context(), 7, nil, tenant.ScopeP2PRelay),
		"no feature_grants row → relay scope not grantable")

	// Env switch off → not grantable regardless of grants.
	hOff := &Handler{cfg: Config{FleetEnabled: false, RelayEnabled: false}, rbac: auth}
	assert.False(t, hOff.scopeGrantable(t.Context(), 7, nil, tenant.ScopeFleet))
	assert.False(t, hOff.scopeGrantable(t.Context(), 7, nil, tenant.ScopeP2PRelay))
}

func TestScopeFeature_maps_known_scopes(t *testing.T) {
	f, ok := scopeFeature(tenant.ScopeFleet)
	require.True(t, ok)
	assert.Equal(t, rbac.FeatureDedicatedServers, f)

	f, ok = scopeFeature(tenant.ScopeP2PRelay)
	require.True(t, ok)
	assert.Equal(t, rbac.FeatureP2PRelay, f)

	_, ok = scopeFeature("mystery")
	assert.False(t, ok)
}

func TestRandomAPIKey_PrefixByType(t *testing.T) {
	cases := []struct {
		keyType    tenant.KeyType
		wantPrefix string
	}{
		{tenant.KeyTypePublishable, "ggp_"},
		{tenant.KeyTypeSecret, "ggs_"},
	}
	for _, tc := range cases {
		t.Run(string(tc.keyType), func(t *testing.T) {
			key, err := randomAPIKey(tc.keyType)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(key, tc.wantPrefix), "key=%q", key)
			assert.Greater(t, len(key), len(tc.wantPrefix)+16)
		})
	}
}

func TestCreateAPIKey_RejectsInvalidKeyType(t *testing.T) {
	// Validation runs before the DB pool is touched, so a nil-pool handler
	// is fine for input-validation assertions. Each case bypasses the
	// pool branch by failing one of the earlier validation gates.
	h := &Handler{}

	cases := []struct {
		name string
		in   createKeyInput
		want error
	}{
		{
			name: "missing_tenant_id",
			in:   createKeyInput{TenantID: 0, KeyType: tenant.KeyTypePublishable, Label: "x"},
			want: errInvalidTenant,
		},
		{
			name: "empty_keytype",
			in:   createKeyInput{TenantID: 1, KeyType: "", Label: "x"},
			want: errInvalidKeyType,
		},
		{
			name: "unknown_keytype",
			in:   createKeyInput{TenantID: 1, KeyType: "admin", Label: "x"},
			want: errInvalidKeyType,
		},
		{
			name: "case_sensitive_secret",
			in:   createKeyInput{TenantID: 1, KeyType: "Secret", Label: "x"},
			want: errInvalidKeyType,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.createAPIKey(context.Background(), 1, tc.in)
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

func TestScopeGrantable_matchmaker_defaults_on(t *testing.T) {
	auth, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	h := &Handler{cfg: Config{}, rbac: auth}
	assert.True(t, h.scopeGrantable(t.Context(), 7, nil, tenant.ScopeMatchmaker),
		"matchmaker scope is grantable with zero config")
}

func TestScopeFeature_maps_matchmaker(t *testing.T) {
	f, ok := scopeFeature(tenant.ScopeMatchmaker)
	require.True(t, ok)
	assert.Equal(t, rbac.FeatureMatchmaker, f)
}
