package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRouter_routes_outside_v1_return_404(t *testing.T) {
	srv := newServer(t)

	cases := []struct {
		name string
		path string
	}{
		{"unprefixed_healthz", "/healthz"},
		{"random_path", "/random/path"},
		{"v2_healthz", "/v2/healthz"},
		{"empty_root", "/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + c.path)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		})
	}
}

func TestRouter_v1_healthz_returns_200_with_version_header(t *testing.T) {
	srv := newServer(t)

	resp, err := http.Get(srv.URL + "/v1/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "v1", resp.Header.Get("X-API-Version"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(body, &payload))
	assert.Equal(t, "ok", payload["status"])
}

func TestRouter_cors_preflight_returns_allow_headers(t *testing.T) {
	srv := newServer(t)

	req, err := http.NewRequest(http.MethodOptions, srv.URL+"/v1/healthz", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://game.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Authorization,Content-Type")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.True(t, resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent,
		"preflight should be 200 or 204, got %d", resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Methods"), "POST")
}

func TestRouter_cors_simple_request_includes_allow_origin_header(t *testing.T) {
	srv := newServer(t)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/healthz", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://game.example.com")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Access-Control-Allow-Origin"))
}

func TestRouter_metrics_endpoint_returns_200(t *testing.T) {
	srv := newServer(t)

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
