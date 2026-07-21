package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/httpapi"
)

func TestRouter_mounts_entitlements_behind_bearer_when_token_set(t *testing.T) {
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Version:             "v1",
		EntitlementAPIToken: strings.Repeat("a", 32),
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/internal/entitlements/1")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"mounted, and unauthenticated requests are refused")
}

func TestRouter_entitlements_absent_by_default(t *testing.T) {
	srv := newServer(t)

	resp, err := http.Get(srv.URL + "/internal/entitlements/1")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
