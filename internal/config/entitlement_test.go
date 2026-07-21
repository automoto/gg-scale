package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateEntitlementAPIToken(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		token   string
		wantErr string
	}{
		{"disabled with no token is the default", false, "", ""},
		{"enabled with no token auto-generates", true, "", ""},
		{"enabled with a long token", true, strings.Repeat("a", 32), ""},
		{"short token rejected", true, "short", "ENTITLEMENT_API_TOKEN"},
		{"token without the switch rejected", false, strings.Repeat("a", 32), "ENTITLEMENT_API_ENABLED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseDev()
			c.EntitlementAPIEnabled = tt.enabled
			c.EntitlementAPIToken = tt.token

			err := c.Validate()

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
		})
	}
}
