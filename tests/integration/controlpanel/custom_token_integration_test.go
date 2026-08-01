//go:build integration

package controlpanel_test

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func customTokenSettingsPath(tenantID int64) string {
	return pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) + "/settings/custom-token"
}

func ed25519PublicPEM(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(cryptorand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func storedCustomTokenKey(t *testing.T, raw *pgxpool.Pool, tenantID int64) string {
	t.Helper()
	var key string
	require.NoError(t, raw.QueryRow(context.Background(),
		`SELECT custom_token_public_key FROM tenants WHERE id = $1`, tenantID).Scan(&key))
	return key
}

func TestCustomTokenKey_save_persists_and_audits(t *testing.T) {
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")
	pemKey := ed25519PublicPEM(t)

	resp, body := tfPostForm(t, admin, srv.URL+customTokenSettingsPath(tenantID),
		url.Values{"_csrf": {csrf}, "custom_token_public_key": {pemKey}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)

	assert.Contains(t, storedCustomTokenKey(t, raw, tenantID), "BEGIN PUBLIC KEY")

	var action string
	require.NoError(t, raw.QueryRow(context.Background(), `
		SELECT action FROM platform_audit_log
		WHERE action = 'control_panel.custom_token_key.update' AND target = $1
		ORDER BY id DESC LIMIT 1`, strconv.FormatInt(tenantID, 10)).Scan(&action))
	assert.Equal(t, "control_panel.custom_token_key.update", action)
}

func TestCustomTokenKey_invalid_key_rejected_and_nothing_persists(t *testing.T) {
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, body := tfPostForm(t, admin, srv.URL+customTokenSettingsPath(tenantID),
		url.Values{"_csrf": {csrf}, "custom_token_public_key": {"not a pem key"}})
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, body, "Paste a PEM-encoded")

	assert.Empty(t, storedCustomTokenKey(t, raw, tenantID))
}

func TestCustomTokenKey_empty_save_clears_the_key(t *testing.T) {
	srv, raw, userID, tenantID, _, _ := newLeaderboardServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "lb-admin@example.com")

	resp, _ := tfPostForm(t, admin, srv.URL+customTokenSettingsPath(tenantID),
		url.Values{"_csrf": {csrf}, "custom_token_public_key": {ed25519PublicPEM(t)}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	resp, _ = tfPostForm(t, admin, srv.URL+customTokenSettingsPath(tenantID),
		url.Values{"_csrf": {csrf}, "custom_token_public_key": {""}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	assert.Empty(t, storedCustomTokenKey(t, raw, tenantID))
}
