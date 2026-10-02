package projectadmin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/tenant"
)

func TestNewAPIKeyValue_prefix_by_type(t *testing.T) {
	cases := map[tenant.KeyType]string{
		tenant.KeyTypePublishable: "ggp_",
		tenant.KeyTypeSecret:      "ggs_",
	}
	for keyType, prefix := range cases {
		key, err := NewAPIKeyValue(keyType)
		require.NoError(t, err)

		assert.True(t, strings.HasPrefix(key, prefix), "key=%q", key)
		assert.Greater(t, len(key), len(prefix)+16)
	}
}

func TestScopeGrantable_should_refuse_unknown_scope(t *testing.T) {
	authz, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)

	ok := ScopeGrantable(t.Context(), authz, KeySwitches{FleetEnabled: true, RelayEnabled: true}, 7, nil, "mystery")

	assert.False(t, ok)
}

// Each scope must follow its own feature grant and no other: a swapped map
// value would grant a scope from the wrong feature.
func TestScopeGrantable_should_follow_only_the_scope_feature(t *testing.T) {
	cases := map[string]rbac.Feature{
		tenant.ScopeFleet:      rbac.FeatureDedicatedServers,
		tenant.ScopeP2PRelay:   rbac.FeatureP2PRelay,
		tenant.ScopeMatchmaker: rbac.FeatureMatchmaker,
	}
	all := []rbac.Feature{rbac.FeatureDedicatedServers, rbac.FeatureP2PRelay, rbac.FeatureMatchmaker}
	switches := KeySwitches{FleetEnabled: true, RelayEnabled: true}
	for scope, feature := range cases {
		t.Run(scope, func(t *testing.T) {
			only := func(on bool) bool {
				authz, err := rbac.NewMemoryAuthorizer()
				require.NoError(t, err)
				for _, f := range all {
					authz.OverrideFeatureForTest(f, (f == feature) == on)
				}
				return ScopeGrantable(t.Context(), authz, switches, 7, nil, scope)
			}

			assert.Equal(t, []bool{true, false}, []bool{only(true), only(false)})
		})
	}
}

func TestScopeGrantable_should_refuse_when_the_server_switch_is_off(t *testing.T) {
	authz, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	authz.OverrideFeatureForTest(rbac.FeatureDedicatedServers, true)
	authz.OverrideFeatureForTest(rbac.FeatureP2PRelay, true)

	got := []bool{
		ScopeGrantable(t.Context(), authz, KeySwitches{}, 7, nil, tenant.ScopeFleet),
		ScopeGrantable(t.Context(), authz, KeySwitches{}, 7, nil, tenant.ScopeP2PRelay),
	}

	assert.Equal(t, []bool{false, false}, got)
}
