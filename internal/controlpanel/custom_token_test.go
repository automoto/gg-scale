package controlpanel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testEd25519PEM(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestTenantSettingsPage_shows_custom_token_card(t *testing.T) {
	pemKey := testEd25519PEM(t)
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, TenantName: "acme",
		CustomTokenPublicKey: pemKey,
	}))
	assert.Contains(t, html, "/v1/control-panel/tenants/3/settings/custom-token")
	assert.Contains(t, html, `name="custom_token_public_key"`)
	assert.Contains(t, html, "BEGIN PUBLIC KEY")
}

func TestTenantSettingsPage_custom_token_card_renders_unconfigured_state(t *testing.T) {
	html := renderToString(t, TenantSettingsPage(TenantSettingsView{
		TenantID: 3, TenantName: "acme",
	}))
	assert.Contains(t, html, `name="custom_token_public_key"`)
	assert.Contains(t, html, "not configured")
}
