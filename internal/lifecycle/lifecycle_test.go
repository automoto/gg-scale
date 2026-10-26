package lifecycle_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/lifecycle"
)

const testToken = "lifecycle-test-token-0123456789abcdef"

// fakeWork is a Drainable whose in-flight count the test controls.
type fakeWork struct {
	lifecycle.Gate
	active atomic.Int64
	begins atomic.Int64
	ends   atomic.Int64
}

func (f *fakeWork) BeginDrain()       { f.begins.Add(1); f.Gate.BeginDrain() }
func (f *fakeWork) EndDrain()         { f.ends.Add(1); f.Gate.EndDrain() }
func (f *fakeWork) ActiveWork() int64 { return f.active.Load() }

func newController(token string, work *fakeWork) *lifecycle.Controller {
	return lifecycle.New(lifecycle.Config{
		Role:         "relay",
		Commit:       "abc1234",
		Token:        token,
		Work:         work,
		Details:      func() map[string]any { return map[string]any{"active_allocations": 3} },
		PollInterval: time.Millisecond,
	})
}

func newMux(c *lifecycle.Controller) *http.ServeMux {
	mux := http.NewServeMux()
	c.Mount(mux)
	return mux
}

func do(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestController_should_start_in_starting(t *testing.T) {
	c := newController(testToken, &fakeWork{})

	assert.Equal(t, lifecycle.StateStarting, c.State())
}

func TestController_should_become_active_when_marked_ready(t *testing.T) {
	c := newController(testToken, &fakeWork{})

	c.MarkReady()

	assert.Equal(t, lifecycle.StateActive, c.State())
}

func TestController_should_begin_drain_once_when_drained_twice(t *testing.T) {
	work := &fakeWork{}
	c := newController(testToken, work)
	c.MarkReady()

	c.Drain()
	c.Drain()

	assert.Equal(t, int64(1), work.begins.Load())
}

func TestController_should_end_drain_once_when_resumed_twice(t *testing.T) {
	work := &fakeWork{}
	c := newController(testToken, work)
	c.MarkReady()
	c.Drain()

	c.Resume()
	c.Resume()

	assert.Equal(t, int64(1), work.ends.Load())
}

func TestController_should_stay_draining_when_ready_arrives_after_drain(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.Drain()

	c.MarkReady()

	assert.Equal(t, lifecycle.StateDraining, c.State())
}

func TestController_should_return_to_starting_when_resumed_before_ready(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.Drain()

	c.Resume()

	assert.Equal(t, lifecycle.StateStarting, c.State())
}

func TestController_should_refuse_resume_when_stopping(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()
	c.Shutdown(time.Millisecond)

	ok := c.Resume()

	assert.False(t, ok)
}

func TestShutdown_should_return_immediately_when_no_work_is_active(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()

	start := time.Now()
	c.Shutdown(time.Minute)

	assert.Less(t, time.Since(start), time.Second)
}

func TestShutdown_should_wait_until_work_reaches_zero(t *testing.T) {
	work := &fakeWork{}
	work.active.Store(2)
	c := newController(testToken, work)
	c.MarkReady()
	go func() {
		time.Sleep(50 * time.Millisecond)
		work.active.Store(0)
	}()

	remaining := c.Shutdown(time.Minute)

	assert.Equal(t, int64(0), remaining)
}

func TestShutdown_should_give_up_when_timeout_elapses(t *testing.T) {
	work := &fakeWork{}
	work.active.Store(5)
	c := newController(testToken, work)
	c.MarkReady()

	start := time.Now()
	remaining := c.Shutdown(50 * time.Millisecond)

	assert.Equal(t, int64(5), remaining)
	assert.Less(t, time.Since(start), time.Second)
}

func TestShutdown_should_end_in_stopping(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()

	c.Shutdown(time.Millisecond)

	assert.Equal(t, lifecycle.StateStopping, c.State())
}

func TestShutdown_should_refuse_new_work_while_waiting(t *testing.T) {
	work := &fakeWork{}
	c := newController(testToken, work)
	c.MarkReady()

	c.Shutdown(time.Millisecond)

	assert.False(t, work.Admit())
}

func TestReadyz_should_report_each_state(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *lifecycle.Controller)
		want  int
	}{
		{"starting", func(*lifecycle.Controller) {}, http.StatusServiceUnavailable},
		{"active", func(c *lifecycle.Controller) { c.MarkReady() }, http.StatusOK},
		{"draining", func(c *lifecycle.Controller) { c.MarkReady(); c.Drain() }, http.StatusServiceUnavailable},
		{"stopping", func(c *lifecycle.Controller) { c.MarkReady(); c.Shutdown(time.Millisecond) }, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newController(testToken, &fakeWork{})
			tt.setup(c)

			rec := do(t, newMux(c), http.MethodGet, "/readyz", "")

			assert.Equal(t, tt.want, rec.Code)
		})
	}
}

func TestLifecycle_should_return_documented_json(t *testing.T) {
	work := &fakeWork{}
	work.active.Store(12)
	c := newController(testToken, work)
	c.MarkReady()
	c.Drain()

	got := decodeStatus(t, do(t, newMux(c), http.MethodGet, "/lifecycle", testToken))

	assert.Equal(t, "relay", got["role"])
	assert.Equal(t, "draining", got["state"])
	assert.Equal(t, "abc1234", got["commit"])
	assert.NotEmpty(t, got["started_at"])
	assert.NotEmpty(t, got["drain_started_at"])
	assert.InDelta(t, 12, got["active_work"], 0)
	assert.Equal(t, map[string]any{"active_allocations": float64(3)}, got["details"])
}

func TestLifecycle_should_report_null_drain_start_when_active(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()

	got := decodeStatus(t, do(t, newMux(c), http.MethodGet, "/lifecycle", testToken))

	assert.Nil(t, got["drain_started_at"])
}

func TestLifecycle_should_report_empty_details_when_none_supplied(t *testing.T) {
	c := lifecycle.New(lifecycle.Config{Role: "api", Work: &fakeWork{}})

	got := decodeStatus(t, do(t, newMux(c), http.MethodGet, "/lifecycle", ""))

	assert.Equal(t, map[string]any{}, got["details"])
}

func TestLifecycleAuth(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		method string
		path   string
		sent   string
		want   int
	}{
		{"get open without configured token", "", http.MethodGet, "/lifecycle", "", http.StatusOK},
		{"get refused without bearer", testToken, http.MethodGet, "/lifecycle", "", http.StatusUnauthorized},
		{"get refused with wrong bearer", testToken, http.MethodGet, "/lifecycle", "wrong", http.StatusUnauthorized},
		{"get allowed with bearer", testToken, http.MethodGet, "/lifecycle", testToken, http.StatusOK},
		{"drain missing without configured token", "", http.MethodPost, "/lifecycle/drain", "", http.StatusNotFound},
		{"resume missing without configured token", "", http.MethodPost, "/lifecycle/resume", "", http.StatusNotFound},
		{"drain refused with wrong bearer", testToken, http.MethodPost, "/lifecycle/drain", "wrong", http.StatusUnauthorized},
		{"resume refused with wrong bearer", testToken, http.MethodPost, "/lifecycle/resume", "wrong", http.StatusUnauthorized},
		{"drain refused without bearer", testToken, http.MethodPost, "/lifecycle/drain", "", http.StatusUnauthorized},
		{"drain rejects get", testToken, http.MethodGet, "/lifecycle/drain", testToken, http.StatusMethodNotAllowed},
		{"readyz open with configured token", testToken, http.MethodGet, "/readyz", "", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newController(tt.token, &fakeWork{})

			rec := do(t, newMux(c), tt.method, tt.path, tt.sent)

			assert.Equal(t, tt.want, rec.Code)
		})
	}
}

func TestDrainRoute_should_move_to_draining(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()

	got := decodeStatus(t, do(t, newMux(c), http.MethodPost, "/lifecycle/drain", testToken))

	assert.Equal(t, "draining", got["state"])
}

func TestDrainRoute_should_return_ok_and_keep_drain_start_when_repeated(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()
	mux := newMux(c)
	first := decodeStatus(t, do(t, mux, http.MethodPost, "/lifecycle/drain", testToken))

	rec := do(t, mux, http.MethodPost, "/lifecycle/drain", testToken)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, first, decodeStatus(t, rec))
}

func TestResumeRoute_should_move_to_active(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()
	c.Drain()

	got := decodeStatus(t, do(t, newMux(c), http.MethodPost, "/lifecycle/resume", testToken))

	assert.Equal(t, "active", got["state"])
}

func TestResumeRoute_should_return_ok_when_repeated(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()
	c.Drain()
	mux := newMux(c)
	do(t, mux, http.MethodPost, "/lifecycle/resume", testToken)

	rec := do(t, mux, http.MethodPost, "/lifecycle/resume", testToken)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "active", decodeStatus(t, rec)["state"])
}

func TestResumeRoute_should_conflict_when_stopping(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	c.MarkReady()
	c.Shutdown(time.Millisecond)

	rec := do(t, newMux(c), http.MethodPost, "/lifecycle/resume", testToken)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestRequireBearer_should_accept_lowercase_scheme(t *testing.T) {
	h := lifecycle.RequireBearer(testToken, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer "+testToken)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMetrics_should_report_current_state(t *testing.T) {
	c := newController(testToken, &fakeWork{})
	reg := prometheus.NewRegistry()
	require.NoError(t, c.Register(reg))
	c.MarkReady()
	c.Drain()

	expected := `
# HELP ggscale_lifecycle_state 1 for the process's current lifecycle state, 0 otherwise.
# TYPE ggscale_lifecycle_state gauge
ggscale_lifecycle_state{state="active"} 0
ggscale_lifecycle_state{state="draining"} 1
ggscale_lifecycle_state{state="starting"} 0
ggscale_lifecycle_state{state="stopping"} 0
`
	assert.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_lifecycle_state"))
}

func TestMetrics_should_count_refused_work(t *testing.T) {
	work := &fakeWork{}
	c := newController(testToken, work)
	reg := prometheus.NewRegistry()
	require.NoError(t, c.Register(reg))
	c.MarkReady()
	c.Drain()

	work.Admit()
	work.Admit()

	expected := `
# HELP ggscale_lifecycle_work_refused_total New work refused because the process was draining.
# TYPE ggscale_lifecycle_work_refused_total counter
ggscale_lifecycle_work_refused_total 2
`
	assert.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_lifecycle_work_refused_total"))
}

func TestGate_should_admit_by_default(t *testing.T) {
	var g lifecycle.Gate

	assert.True(t, g.Admit())
}

func TestGate_should_admit_again_after_drain_ends(t *testing.T) {
	var g lifecycle.Gate
	g.BeginDrain()

	g.EndDrain()

	assert.True(t, g.Admit())
}

func TestInflight_should_count_request_in_progress(t *testing.T) {
	var inflight lifecycle.Inflight
	var during int64
	h := inflight.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		during = inflight.Count()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, int64(1), during)
}

func TestInflight_should_release_count_when_request_ends(t *testing.T) {
	var inflight lifecycle.Inflight
	h := inflight.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, int64(0), inflight.Count())
}

func TestInflight_should_skip_websocket_upgrades(t *testing.T) {
	var inflight lifecycle.Inflight
	var during int64
	h := inflight.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		during = inflight.Count()
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
	req.Header.Set("Upgrade", "websocket")

	h.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, int64(0), during)
}
