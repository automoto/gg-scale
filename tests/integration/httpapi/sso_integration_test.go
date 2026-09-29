//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/players"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/sso"
	"github.com/automoto/gg-scale/internal/sso/ssotest"
	"github.com/automoto/gg-scale/internal/tenant"
)

// newSSOServer mounts both surfaces in the full router on the application
// database role, so the table grants and the route order are the real ones.
func newSSOServer(t *testing.T, c *cluster) (*httptest.Server, *ssotest.Fake) {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	fake := ssotest.New(t)

	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	google := func(ssoPath string) map[string]sso.Provider {
		p := sso.Google("client-id", "client-secret", srv.URL+ssoPath+"/google/callback")
		p.OAuth.Endpoint = fake.Endpoint()
		p.ProfileURL = fake.ProfileURL()
		return map[string]sso.Provider{"google": p}
	}
	handler = httpapi.NewRouter(httpapi.Deps{
		Version:               "v1",
		Commit:                "test",
		Pool:                  db.NewPool(c.appPool),
		Lookup:                tenant.NewSQLLookup(c.appPool),
		Limiter:               ratelimit.NewCacheLimiter(c.cache),
		Signer:                signer,
		Cache:                 c.cache,
		Mailer:                &mailer.Recorder{},
		MailFrom:              "no-reply@example.test",
		EmailVerifySigningKey: []byte(testEmailVerifySigningKey),
		ControlPanel: controlpanel.Config{
			Mount:        true,
			SSOProviders: google("/v1/control-panel/sso"),
		},
		ControlPanelBootstrap: controlpanel.DisabledBootstrap(),
		Players: players.Config{
			Mount:        true,
			SSOProviders: google("/v1/players/account/sso"),
		},
	})
	return srv, fake
}

// ssoCallback posts the start form, visits the provider, and returns the
// response of the callback.
func ssoCallback(t *testing.T, client *http.Client, startURL string, form url.Values) *http.Response {
	t.Helper()
	resp, err := client.PostForm(startURL, form)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, err = client.Get(resp.Header.Get("Location"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusFound, resp.StatusCode)
	resp, err = client.Get(resp.Header.Get("Location"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp
}

func TestSSO_player_routes_work_in_the_full_router_on_the_app_role(t *testing.T) {
	c := startCluster(t)
	srv, fake := newSSOServer(t, c)
	base := srv.URL + "/v1/players/account"
	fake.SetProfile(`{"sub":"g-player","email":"player@example.com","email_verified":true,"name":"Pat"}`)
	client := jarClient(t)
	csrf := getPlayerCSRF(t, client, base+"/login")

	signIn := ssoCallback(t, client, base+"/sso/google/start", url.Values{"_csrf": {csrf}})
	unlink, err := client.PostForm(base+"/sso/google/unlink", url.Values{"_csrf": {csrf}})
	require.NoError(t, err)
	require.NoError(t, unlink.Body.Close())

	assert.Equal(t, "/v1/players/account/", signIn.Header.Get("Location"))
	// The last sign-in method stays: the unlink ran the row lock and the
	// count on the application role, then refused.
	assert.Contains(t, unlink.Header.Get("Location"), "/v1/players/account/?error=")
	var connections, audits int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM player_account_connections c
		 JOIN player_accounts a ON a.id = c.player_account_id
		 WHERE a.email = 'player@example.com' AND a.password_hash IS NULL`).Scan(&connections))
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log WHERE action = 'sso.signup' AND actor_service = 'player_sso'`).Scan(&audits))
	assert.Equal(t, 1, connections)
	assert.Equal(t, 1, audits)
}

func TestSSO_control_panel_routes_work_in_the_full_router_on_the_app_role(t *testing.T) {
	c := startCluster(t)
	srv, fake := newSSOServer(t, c)
	userID := seedControlPanelUser(t, c, "op@example.com", "correct-horse-battery-staple", false)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO control_panel_user_connections (control_panel_user_id, provider, subject)
		 VALUES ($1, 'google', 'g-op')`, userID)
	require.NoError(t, err)
	fake.SetProfile(`{"sub":"g-op","email":"op@example.com","email_verified":true}`)
	client := jarClient(t)

	signIn := ssoCallback(t, client, srv.URL+"/v1/control-panel/login/sso/google/start", url.Values{})

	assert.Equal(t, "/v1/control-panel", signIn.Header.Get("Location"))
	var used int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM control_panel_user_connections
		 WHERE control_panel_user_id = $1 AND last_used_at IS NOT NULL`, userID).Scan(&used))
	assert.Equal(t, 1, used)
}
