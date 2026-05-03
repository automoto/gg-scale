package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ggscale/ggscale/internal/dashboard"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDashboardServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
		Dashboard: dashboard.Config{
			Mount: true,
		},
		DashboardBootstrap: dashboard.NewBootstrap("setup-token"),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDashboard_login_page_renders_under_v1(t *testing.T) {
	srv := newDashboardServer(t)

	resp, err := http.Get(srv.URL + "/v1/dashboard/login")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "ggscale Dashboard")
	assert.Contains(t, string(body), `name="email"`)
	assert.Contains(t, string(body), `src="/v1/dashboard/assets/htmx.min.js"`)
}

func TestDashboard_setup_page_renders_when_bootstrap_pending(t *testing.T) {
	srv := newDashboardServer(t)

	resp, err := http.Get(srv.URL + "/v1/dashboard/setup?token=setup-token")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "Set up ggscale Dashboard")
	assert.Contains(t, string(body), `name="bootstrap_token" value="setup-token"`)
}

func TestDashboard_routes_outside_v1_return_404(t *testing.T) {
	srv := newDashboardServer(t)

	resp, err := http.Get(srv.URL + "/dashboard")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDashboard_login_page_sets_security_headers(t *testing.T) {
	srv := newDashboardServer(t)

	resp, err := http.Get(srv.URL + "/v1/dashboard/login")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "same-origin", resp.Header.Get("Referrer-Policy"))
}

func TestDashboard_requires_login_for_home(t *testing.T) {
	srv := newDashboardServer(t)
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(srv.URL + "/v1/dashboard")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/dashboard/login", resp.Header.Get("Location"))
}
