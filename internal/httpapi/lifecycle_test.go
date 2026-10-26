package httpapi_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/lifecycle"
	"github.com/automoto/gg-scale/internal/realtime"
)

const apiLifecycleTestToken = "api-lifecycle-token-0123456789abcdef"

var _ lifecycle.Drainable = httpapi.APIWork{}

type apiLifecycleFixture struct {
	work   httpapi.APIWork
	lc     *lifecycle.Controller
	router http.Handler
}

func newAPILifecycleFixture(t *testing.T) apiLifecycleFixture {
	t.Helper()
	work := httpapi.APIWork{Realtime: realtime.NewDrain(), Inflight: &lifecycle.Inflight{}}
	lc := lifecycle.New(lifecycle.Config{
		Role: "api", Commit: "test", Token: apiLifecycleTestToken, Work: work, Details: work.Details,
	})
	reg := prometheus.NewRegistry()
	require.NoError(t, lc.Register(reg))
	router := work.Inflight.Middleware(httpapi.NewRouter(httpapi.Deps{
		Version: "v1", Commit: "test", Registry: reg, Lifecycle: lc, RealtimeDrain: work.Realtime,
	}))
	return apiLifecycleFixture{work: work, lc: lc, router: router}
}

func (f apiLifecycleFixture) do(method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestAPILifecycle_should_serve_readyz_at_root(t *testing.T) {
	f := newAPILifecycleFixture(t)
	f.lc.MarkReady()

	rec := f.do(http.MethodGet, "/readyz", "")

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAPILifecycle_should_turn_readyz_503_after_drain(t *testing.T) {
	f := newAPILifecycleFixture(t)
	f.lc.MarkReady()

	f.do(http.MethodPost, "/lifecycle/drain", apiLifecycleTestToken)

	assert.Equal(t, http.StatusServiceUnavailable, f.do(http.MethodGet, "/readyz", "").Code)
}

func TestAPILifecycle_should_keep_serving_healthz_while_draining(t *testing.T) {
	f := newAPILifecycleFixture(t)
	f.lc.MarkReady()
	f.lc.Drain()

	rec := f.do(http.MethodGet, "/v1/healthz", "")

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAPILifecycle_should_report_api_role_and_details(t *testing.T) {
	f := newAPILifecycleFixture(t)
	f.lc.MarkReady()

	rec := f.do(http.MethodGet, "/lifecycle", apiLifecycleTestToken)

	var got struct {
		Role       string         `json:"role"`
		ActiveWork int64          `json:"active_work"`
		Details    map[string]any `json:"details"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "api", got.Role)
	assert.Equal(t, map[string]any{"realtime_connections": float64(0), "inflight_requests": float64(0)}, got.Details)
}

func TestAPILifecycle_should_report_zero_active_work_when_idle(t *testing.T) {
	f := newAPILifecycleFixture(t)
	f.lc.MarkReady()

	rec := f.do(http.MethodGet, "/lifecycle", apiLifecycleTestToken)

	var got struct {
		ActiveWork int64 `json:"active_work"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, int64(0), got.ActiveWork)
}

func TestAPILifecycle_should_refuse_lifecycle_without_token(t *testing.T) {
	f := newAPILifecycleFixture(t)

	rec := f.do(http.MethodGet, "/lifecycle", "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAPILifecycle_should_expose_lifecycle_state_metric(t *testing.T) {
	f := newAPILifecycleFixture(t)
	f.lc.MarkReady()

	body, err := io.ReadAll(f.do(http.MethodGet, "/metrics", "").Body)
	require.NoError(t, err)

	assert.Contains(t, string(body), `ggscale_lifecycle_state{state="active"} 1`)
}

func TestAPILifecycle_should_not_mount_routes_without_controller(t *testing.T) {
	router := httpapi.NewRouter(httpapi.Deps{Version: "v1", Commit: "test"})
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAPIWork_should_drain_realtime_upgrades(t *testing.T) {
	work := httpapi.APIWork{Realtime: realtime.NewDrain(), Inflight: &lifecycle.Inflight{}}

	work.BeginDrain()

	assert.True(t, work.Realtime.Draining())
}

func TestAPIWork_should_resume_realtime_upgrades(t *testing.T) {
	work := httpapi.APIWork{Realtime: realtime.NewDrain(), Inflight: &lifecycle.Inflight{}}
	work.BeginDrain()

	work.EndDrain()

	assert.False(t, work.Realtime.Draining())
}
