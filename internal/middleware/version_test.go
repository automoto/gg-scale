package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ggscale/ggscale/internal/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestVersionMiddleware_sets_response_header(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := middleware.NewVersion("v1", reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, "v1", rec.Header().Get("X-API-Version"))
}

func TestVersionMiddleware_increments_request_counter(t *testing.T) {
	reg := prometheus.NewRegistry()
	mw := middleware.NewVersion("v1", reg)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/anything", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	expected := strings.NewReader(`
# HELP ggscale_http_requests_by_version_total HTTP requests handled, labeled by API version.
# TYPE ggscale_http_requests_by_version_total counter
ggscale_http_requests_by_version_total{version="v1"} 2
`)
	assert.NoError(t, testutil.GatherAndCompare(reg, expected, "ggscale_http_requests_by_version_total"))
}
