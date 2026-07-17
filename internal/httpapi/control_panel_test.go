package httpapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ggscale/ggscale/internal/controlpanel"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newControlPanelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version:               "v1",
		Commit:                "test",
		EmailVerifySigningKey: []byte("0123456789abcdef0123456789abcdef"),
		ControlPanel: controlpanel.Config{
			Mount: true,
		},
		ControlPanelBootstrap: controlpanel.NewBootstrap("setup-token", "/tmp/ggscale-bootstrap.token"),
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestControlPanel_login_page_renders_under_v1(t *testing.T) {
	srv := newControlPanelServer(t)

	resp, err := http.Get(srv.URL + "/v1/control-panel/login")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "ggscale Control panel")
	assert.Contains(t, string(body), `name="email"`)
	assert.Contains(t, string(body), `src="/v1/control-panel/assets/htmx.min.js?v=`)
}

func TestControlPanel_setup_page_renders_when_bootstrap_pending(t *testing.T) {
	srv := newControlPanelServer(t)

	resp, err := http.Get(srv.URL + "/v1/control-panel/setup")
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "Set up ggscale")
	assert.Contains(t, string(body), `name="bootstrap_token"`)
	assert.NotContains(t, string(body), `value="setup-token"`)
}

func TestSharedAssets_served_under_v1(t *testing.T) {
	srv := newControlPanelServer(t)

	for _, name := range []string{"pico.min.css", "app.css"} {
		resp, err := http.Get(srv.URL + "/v1/assets/" + name)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, resp.StatusCode, name)
		assert.Contains(t, resp.Header.Get("Content-Type"), "text/css", name)
		assert.NotZero(t, len(body), name)
	}
}

func TestLegacyFaviconRouteServesEmbeddedIcon(t *testing.T) {
	srv := newControlPanelServer(t)

	resp, err := http.Get(srv.URL + "/favicon.ico")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "image/svg+xml")
	assert.Contains(t, resp.Header.Get("Cache-Control"), "immutable")
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.NotEmpty(t, body)
}

func TestControlPanel_routes_outside_v1_return_404(t *testing.T) {
	srv := newControlPanelServer(t)

	resp, err := http.Get(srv.URL + "/control-panel")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestControlPanel_login_page_sets_security_headers(t *testing.T) {
	srv := newControlPanelServer(t)

	resp, err := http.Get(srv.URL + "/v1/control-panel/login")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	assert.Equal(t, "same-origin", resp.Header.Get("Referrer-Policy"))
}

func TestControlPanel_requires_login_for_home(t *testing.T) {
	srv := newControlPanelServer(t)
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(srv.URL + "/v1/control-panel")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/v1/control-panel/login", resp.Header.Get("Location"))
}
