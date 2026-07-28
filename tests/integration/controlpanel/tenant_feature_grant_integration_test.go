//go:build integration

package controlpanel_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/rbac"
)

// newFeatureGrantServer brings up the control panel with the relay and fleet
// startup switches on, seeded with one platform-admin user and one tenant, so
// the admin feature-grant form is offered and its POST is accepted.
func newFeatureGrantServer(t *testing.T) (srv *httptest.Server, raw *pgxpool.Pool, userID, tenantID int64) {
	t.Helper()
	pool, raw := startTwoFactorDB(t)
	ctx := context.Background()

	userID = createPlatformAdminUser(t, raw, "fg-admin@example.com")
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('fg-test') RETURNING id`).Scan(&tenantID))

	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)

	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{
		Pool: pool,
		Config: controlpanel.Config{
			Mount: true, RelayEnabled: true, FleetEnabled: true,
		},
		Mailer: noopMailer, RBAC: authorizer,
		VerifySigningKey: []byte(testEmailVerifySigningKey),
	}))
	srv = httptest.NewServer(root)
	t.Cleanup(srv.Close)
	return srv, raw, userID, tenantID
}

func featuresPath(tenantID int64) string {
	return pathControlPanel + "/tenants/" + strconv.FormatInt(tenantID, 10) + "/settings/features"
}

func featureGrantEnabled(t *testing.T, raw *pgxpool.Pool, tenantID int64, feature string) (bool, bool) {
	t.Helper()
	var enabled bool
	err := raw.QueryRow(context.Background(),
		`SELECT enabled FROM feature_grants
		 WHERE tenant_id = $1 AND project_id IS NULL AND feature = $2`,
		tenantID, feature).Scan(&enabled)
	if err != nil {
		return false, false // no row
	}
	return enabled, true
}

func TestPlatformAdmin_can_grant_and_revoke_feature(t *testing.T) {
	srv, raw, userID, tenantID := newFeatureGrantServer(t)
	ctx := context.Background()
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "fg-admin@example.com")

	// The settings page renders the admin feature-grant form for a platform admin.
	_, page := tfGet(t, admin, srv.URL+pathControlPanel+"/tenants/"+strconv.FormatInt(tenantID, 10)+"/settings")
	assert.Contains(t, page, featuresPath(tenantID))
	assert.Contains(t, page, `name="feature" value="p2p_relay"`)

	// Enable p2p_relay.
	resp, _ := tfPostForm(t, admin, srv.URL+featuresPath(tenantID),
		url.Values{"_csrf": {csrf}, "feature": {"p2p_relay"}, "enabled": {"on"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	enabled, found := featureGrantEnabled(t, raw, tenantID, "p2p_relay")
	require.True(t, found, "grant row should exist after enable")
	assert.True(t, enabled)

	var actorID int64
	var auditFeature, auditEnabled string
	require.NoError(t, raw.QueryRow(ctx, `
		SELECT actor_user_id, payload->>'feature', payload->>'enabled'
		FROM platform_audit_log
		WHERE action = 'control_panel.tenant.feature_grant' AND target = $1
		ORDER BY id DESC LIMIT 1`, strconv.FormatInt(tenantID, 10)).
		Scan(&actorID, &auditFeature, &auditEnabled))
	assert.Equal(t, userID, actorID)
	assert.Equal(t, "p2p_relay", auditFeature)
	assert.Equal(t, "true", auditEnabled)

	// Disable it again.
	resp, _ = tfPostForm(t, admin, srv.URL+featuresPath(tenantID),
		url.Values{"_csrf": {csrf}, "feature": {"p2p_relay"}, "enabled": {"off"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	enabled, found = featureGrantEnabled(t, raw, tenantID, "p2p_relay")
	require.True(t, found, "grant row survives a revoke (soft-disable)")
	assert.False(t, enabled)

	require.NoError(t, raw.QueryRow(ctx, `
		SELECT payload->>'enabled' FROM platform_audit_log
		WHERE action = 'control_panel.tenant.feature_grant' AND target = $1
		ORDER BY id DESC LIMIT 1`, strconv.FormatInt(tenantID, 10)).Scan(&auditEnabled))
	assert.Equal(t, "false", auditEnabled)
}

func TestPlatformAdmin_feature_grant_rejects_soft_deleted_tenant(t *testing.T) {
	srv, raw, userID, tenantID := newFeatureGrantServer(t)
	ctx := context.Background()
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "fg-admin@example.com")
	_, err := raw.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenantID)
	require.NoError(t, err)

	resp, _ := tfPostForm(t, admin, srv.URL+featuresPath(tenantID),
		url.Values{"_csrf": {csrf}, "feature": {"p2p_relay"}, "enabled": {"on"}})

	// A deleted tenant must not regain a paid feature.
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_, found := featureGrantEnabled(t, raw, tenantID, "p2p_relay")
	assert.False(t, found)
}

func TestPlatformAdmin_feature_grant_rejects_unknown_feature(t *testing.T) {
	srv, raw, userID, tenantID := newFeatureGrantServer(t)
	admin, csrf := loginAsAdmin(t, srv, raw, userID, "fg-admin@example.com")

	resp, _ := tfPostForm(t, admin, srv.URL+featuresPath(tenantID),
		url.Values{"_csrf": {csrf}, "feature": {"matchmaker"}, "enabled": {"on"}})
	// Redirected back with a flash; no grant row is written for a
	// non-directly-grantable feature.
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, found := featureGrantEnabled(t, raw, tenantID, "matchmaker")
	assert.False(t, found)
}
