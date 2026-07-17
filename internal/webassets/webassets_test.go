package webassets_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/webassets"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	webassets.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandlerServesSharedStylesheets(t *testing.T) {
	for _, name := range []string{"/pico.min.css", "/app.css"} {
		rec := get(t, name)
		require.Equal(t, http.StatusOK, rec.Code, name)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/css", name)
		assert.Contains(t, rec.Header().Get("Cache-Control"), "immutable", name)
	}
}

func TestHandlerServesFonts(t *testing.T) {
	rec := get(t, "/fonts/inter-variable.woff2")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotZero(t, rec.Body.Len())
}

func TestHandlerServesVersionedFaviconWithSecurityHeaders(t *testing.T) {
	rec := get(t, "/favicon.svg?v=ignored")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "image/svg+xml")
	assert.Contains(t, rec.Header().Get("Cache-Control"), "immutable")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.True(t, strings.HasPrefix(webassets.URL("favicon.svg"), "/v1/assets/favicon.svg?v="))
}

func TestHandlerRejectsUnknownAsset(t *testing.T) {
	rec := get(t, "/does-not-exist.css")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestURL_appends_content_version_for_known_assets(t *testing.T) {
	u := webassets.URL("app.css")
	require.True(t, strings.HasPrefix(u, "/v1/assets/app.css?v="), u)
	// The version is a stable content hash: same input, same URL.
	assert.Equal(t, u, webassets.URL("app.css"))
	assert.NotEqual(t, u, webassets.URL("pico.min.css"))
}

func TestURL_leaves_unknown_assets_unversioned(t *testing.T) {
	assert.Equal(t, "/v1/assets/nope.css", webassets.URL("nope.css"))
}

func TestHandlerIgnoresVersionQuery(t *testing.T) {
	rec := get(t, "/app.css?v=00000000")
	assert.Equal(t, http.StatusOK, rec.Code)
}
