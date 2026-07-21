package controlpanel

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/billing"
)

func TestTenantSettingsPage_renders_billing_links_when_configured(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, TenantName: "acme", Tier: "tier_0",
		BillingPortalURL:    "https://billing.example.com/portal",
		BillingUpgradeURL:   "https://billing.example.com/start",
		BillingUpgradeToken: "signed-token",
	}))

	assert.Contains(t, html, "https://billing.example.com/portal")
	assert.Contains(t, html, "https://billing.example.com/start?t=signed-token")
	assert.Contains(t, html, "Manage billing")
	assert.Contains(t, html, "Upgrade")
}

func TestTenantSettingsPage_omits_billing_links_by_default(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, TenantName: "acme", Tier: "tier_0",
	}))

	assert.NotContains(t, html, "Manage billing")
	assert.NotContains(t, html, "billing.example.com")
}

func TestBillingLinks_mints_a_verifiable_handoff_token(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Unix(1_700_000_000, 0)
	h := &Handler{
		cfg:               Config{BillingUpgradeURL: "https://billing.example.com/start"},
		billingHandoffKey: key,
		now:               func() time.Time { return now },
	}

	upgradeURL, token := h.billingLinks(42)

	assert.Equal(t, "https://billing.example.com/start", upgradeURL)
	tenantID, err := billing.VerifyHandoff(key, token, now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, int64(42), tenantID)
}

func TestBillingLinks_absent_without_key_or_url(t *testing.T) {
	h := &Handler{cfg: Config{BillingUpgradeURL: "https://billing.example.com/start"}}
	url, token := h.billingLinks(42)
	assert.Empty(t, url, "no key loaded means no upgrade link")
	assert.Empty(t, token)

	h = &Handler{billingHandoffKey: []byte("0123456789abcdef0123456789abcdef")}
	url, token = h.billingLinks(42)
	assert.Empty(t, url, "no upgrade URL configured means no link")
	assert.Empty(t, token)
}
