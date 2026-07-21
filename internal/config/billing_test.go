package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ggscale/ggscale/internal/config"
)

func baseDev() *config.Config {
	return &config.Config{
		Env:        "dev",
		DBMaxConns: 10,
		DBMinConns: 2,
	}
}

func TestValidateBillingPortalURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty means feature off", "", false},
		{"https URL accepted", "https://billing.example.com/portal", false},
		{"http URL accepted for dev", "http://localhost:9090/checkout", false},
		{"plain text rejected", "not a url", true},
		{"missing host rejected", "https://", true},
		{"non-http scheme rejected", "ftp://billing.example.com", true},
		{"relative path rejected", "/portal", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseDev()
			c.BillingPortalURL = tt.url

			err := c.Validate()

			if tt.wantErr {
				assert.ErrorContains(t, err, "BILLING_PORTAL_URL")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateBillingUpgradeURL(t *testing.T) {
	c := baseDev()
	c.BillingUpgradeURL = "https://billing.example.com/start"
	assert.NoError(t, c.Validate())

	c.BillingUpgradeURL = "not a url"
	assert.ErrorContains(t, c.Validate(), "BILLING_UPGRADE_URL")
}

func TestValidateBillingURLsRequireHTTPSInProduction(t *testing.T) {
	c := baseProd()
	c.BillingPortalURL = "http://billing.example.com/portal"
	assert.ErrorContains(t, c.Validate(), "BILLING_PORTAL_URL")

	c = baseProd()
	c.BillingUpgradeURL = "http://billing.example.com/start"
	assert.ErrorContains(t, c.Validate(), "BILLING_UPGRADE_URL")
}

func TestValidateBillingHandoffKey(t *testing.T) {
	c := baseDev()
	c.BillingUpgradeURL = "https://billing.example.com/start"
	c.BillingHandoffKey = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	assert.NoError(t, c.Validate())

	c.BillingHandoffKey = "deadbeef"
	assert.ErrorContains(t, c.Validate(), "BILLING_HANDOFF_KEY")

	c = baseDev()
	c.BillingHandoffKey = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	assert.ErrorContains(t, c.Validate(), "BILLING_UPGRADE_URL",
		"handoff key without the upgrade URL is a misconfiguration")
}
