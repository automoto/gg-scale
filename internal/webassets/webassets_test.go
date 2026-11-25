package webassets_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/webassets"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	webassets.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandlerServesSharedStylesheets(t *testing.T) {
	for _, name := range []string{"pico.min.css", "app.css"} {
		rec := get(t, strings.TrimPrefix(webassets.URL(name), "/v1/assets"))
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
	rec := get(t, strings.TrimPrefix(webassets.URL("favicon.svg"), "/v1/assets"))

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

func TestHandler_should_not_send_immutable_when_version_does_not_match(t *testing.T) {
	rec := get(t, "/app.css?v=00000000")

	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
}

func TestHandler_should_send_immutable_when_version_matches(t *testing.T) {
	rec := get(t, strings.TrimPrefix(webassets.URL("app.css"), "/v1/assets"))

	assert.Contains(t, rec.Header().Get("Cache-Control"), "immutable")
}

func TestHandler_should_send_no_cache_when_request_has_no_version(t *testing.T) {
	rec := get(t, "/fonts/inter-variable.woff2")

	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
}

func TestHandler_should_answer_not_modified_when_the_browser_copy_is_current(t *testing.T) {
	etag := get(t, "/fonts/inter-variable.woff2").Header().Get("ETag")
	req := httptest.NewRequest(http.MethodGet, "/fonts/inter-variable.woff2", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()

	webassets.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotModified, rec.Code)
}
