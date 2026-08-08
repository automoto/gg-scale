//go:build integration

package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
)

func testEd25519PublicKeyPEM(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// The save/clear UPDATE runs as ggscale_app under the tenants RLS policies,
// which only admit writes in the tenant's GUC scope — a bootstrap-scoped
// write matches zero rows and surfaces as a 404.
func TestCustomTokenKey_save_and_clear_under_rls(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "ct-key")
	userID := seedControlPanelUser(t, c, "ct-admin@example.com", "admin-password-1", false)
	seedControlPanelMembership(t, c, userID, tenantID, "owner")
	srv, _ := newControlPanelAndPlayerServerWithLimiter(t, c, controlpanel.Config{
		Mount:    true,
		BaseURL:  "http://app.example.test",
		MailFrom: "no-reply@example.test",
	}, branchAllowAllLimiter{})

	cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "ct-admin@example.com", "admin-password-1")
	postKey := func(pemKey string) int {
		form := url.Values{"_csrf": {csrf}, "custom_token_public_key": {pemKey}}
		req, err := http.NewRequest(http.MethodPost,
			fmt.Sprintf("%s/v1/control-panel/tenants/%d/settings/custom-token", srv.URL, tenantID),
			strings.NewReader(form.Encode()))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		resp, err := noRedirectClient().Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}

	pemKey := testEd25519PublicKeyPEM(t)
	require.Equal(t, http.StatusSeeOther, postKey(pemKey), "saving the key must succeed under RLS")
	var stored string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT custom_token_public_key FROM tenants WHERE id = $1`, tenantID).Scan(&stored))
	assert.Equal(t, strings.TrimSpace(pemKey)+"\n", stored)

	require.Equal(t, http.StatusSeeOther, postKey(""), "clearing the key must succeed under RLS")
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT custom_token_public_key FROM tenants WHERE id = $1`, tenantID).Scan(&stored))
	assert.Empty(t, stored)
}
