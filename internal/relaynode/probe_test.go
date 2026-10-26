package relaynode_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relaynode"
)

const probeTestToken = "relay-probe-token-0123456789abcdef"

func lifecycleServer(t *testing.T, code int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const activeRelayBody = `{"role":"relay","state":"active","active_work":3,"details":{"active_allocations":3}}`

func TestProbe_should_succeed_for_an_active_relay(t *testing.T) {
	srv := lifecycleServer(t, http.StatusOK, activeRelayBody)
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	work := int64(3)
	assert.Equal(t, relaynode.Result{OK: true, State: "active", ActiveWork: &work}, got)
}

func TestProbe_should_succeed_for_a_draining_relay(t *testing.T) {
	srv := lifecycleServer(t, http.StatusOK, `{"role":"relay","state":"draining","active_work":1}`)
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	assert.True(t, got.OK)
}

func TestProbe_should_fail_for_a_process_that_is_not_a_relay(t *testing.T) {
	srv := lifecycleServer(t, http.StatusOK, `{"role":"api","state":"active","active_work":0}`)
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	assert.False(t, got.OK)
}

func TestProbe_should_fail_for_an_unknown_state(t *testing.T) {
	srv := lifecycleServer(t, http.StatusOK, `{"role":"relay","state":"sleeping","active_work":0}`)
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	assert.Equal(t, relaynode.Result{Failure: `unknown lifecycle state "sleeping"`}, got)
}

func TestProbe_should_fail_for_a_non_2xx_status(t *testing.T) {
	srv := lifecycleServer(t, http.StatusServiceUnavailable, activeRelayBody)
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	assert.False(t, got.OK)
}

func TestProbe_should_fail_for_malformed_json(t *testing.T) {
	srv := lifecycleServer(t, http.StatusOK, `not json`)
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	assert.False(t, got.OK)
}

func TestProbe_should_flag_a_401_as_unauthorized(t *testing.T) {
	srv := lifecycleServer(t, http.StatusUnauthorized, "unauthorized")
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	assert.True(t, got.Unauthorized)
}

func TestProbe_should_fail_when_the_node_times_out(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	p := relaynode.NewProber("", 50*time.Millisecond)

	got := p.Probe(t.Context(), srv.URL)

	assert.False(t, got.OK)
}

func TestProbe_should_not_follow_redirects(t *testing.T) {
	target := lifecycleServer(t, http.StatusOK, activeRelayBody)
	srv := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	t.Cleanup(srv.Close)
	p := relaynode.NewProber("", time.Second)

	got := p.Probe(t.Context(), srv.URL)

	assert.False(t, got.OK)
}

func TestProbe_should_send_the_probe_token(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, activeRelayBody)
	}))
	t.Cleanup(srv.Close)
	p := relaynode.NewProber(probeTestToken, time.Second)

	p.Probe(t.Context(), srv.URL)

	assert.Equal(t, "Bearer "+probeTestToken, auth)
}

func TestProbe_should_send_no_authorization_without_a_token(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Values("Authorization")
		_, _ = io.WriteString(w, activeRelayBody)
	}))
	t.Cleanup(srv.Close)
	p := relaynode.NewProber("", time.Second)

	p.Probe(t.Context(), srv.URL)

	assert.Empty(t, auth)
}

// fakeProbeStore is an in-memory ProbeStore.
type fakeProbeStore struct {
	mu      sync.Mutex
	nodes   []relaynode.Node
	results map[int64]relaynode.Result
}

func (s *fakeProbeStore) Known(context.Context) ([]relaynode.Node, error) { return s.nodes, nil }

func (s *fakeProbeStore) RecordProbe(_ context.Context, id int64, r relaynode.Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.results == nil {
		s.results = map[int64]relaynode.Result{}
	}
	s.results[id] = r
	return nil
}

func TestProbeAll_should_record_a_result_for_every_known_node(t *testing.T) {
	up := lifecycleServer(t, http.StatusOK, activeRelayBody)
	down := lifecycleServer(t, http.StatusServiceUnavailable, "")
	store := &fakeProbeStore{nodes: []relaynode.Node{{ID: 1, LifecycleURL: up.URL}, {ID: 2, LifecycleURL: down.URL}}}
	m := relaynode.NewMonitor(store, relaynode.NewProber("", time.Second), slog.New(slog.DiscardHandler))

	require.NoError(t, m.ProbeAll(t.Context()))

	assert.Equal(t, map[int64]bool{1: true, 2: false}, map[int64]bool{1: store.results[1].OK, 2: store.results[2].OK})
}

func TestProbeAll_should_warn_about_a_token_mismatch_once_per_node(t *testing.T) {
	srv := lifecycleServer(t, http.StatusUnauthorized, "unauthorized")
	store := &fakeProbeStore{nodes: []relaynode.Node{{ID: 1, LifecycleURL: srv.URL}}}
	var logs bytes.Buffer
	m := relaynode.NewMonitor(store, relaynode.NewProber("", time.Second), slog.New(slog.NewTextHandler(&logs, nil)))

	for range 3 {
		require.NoError(t, m.ProbeAll(t.Context()))
	}

	assert.Equal(t, 1, strings.Count(logs.String(), "RELAY_PROBE_TOKEN"))
}

func TestProbeAll_should_warn_again_after_the_node_recovers(t *testing.T) {
	var code int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(code)
		_, _ = io.WriteString(w, activeRelayBody)
	}))
	t.Cleanup(srv.Close)
	setCode := func(c int) {
		mu.Lock()
		defer mu.Unlock()
		code = c
	}
	store := &fakeProbeStore{nodes: []relaynode.Node{{ID: 1, LifecycleURL: srv.URL}}}
	var logs bytes.Buffer
	m := relaynode.NewMonitor(store, relaynode.NewProber("", time.Second), slog.New(slog.NewTextHandler(&logs, nil)))

	for _, c := range []int{http.StatusUnauthorized, http.StatusOK, http.StatusUnauthorized} {
		setCode(c)
		require.NoError(t, m.ProbeAll(t.Context()))
	}

	assert.Equal(t, 2, strings.Count(logs.String(), "RELAY_PROBE_TOKEN"))
}

func TestProbeAll_should_log_a_repeated_failure_once(t *testing.T) {
	srv := lifecycleServer(t, http.StatusServiceUnavailable, "")
	store := &fakeProbeStore{nodes: []relaynode.Node{{ID: 1, LifecycleURL: srv.URL}}}
	var logs bytes.Buffer
	m := relaynode.NewMonitor(store, relaynode.NewProber("", time.Second), slog.New(slog.NewTextHandler(&logs, nil)))

	for range 3 {
		require.NoError(t, m.ProbeAll(t.Context()))
	}

	assert.Equal(t, 1, strings.Count(logs.String(), "relay node probe failed"))
}
