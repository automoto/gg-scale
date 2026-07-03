package dashboard

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/tenant"
)

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
