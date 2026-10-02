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
