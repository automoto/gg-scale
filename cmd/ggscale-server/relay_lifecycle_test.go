package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/config"
	"github.com/automoto/gg-scale/internal/relay"
)

const relayLifecycleTestToken = "relay-lifecycle-token-0123456789abcdef"

func newTestRelayServer(t *testing.T) *relay.Server {
	t.Helper()
	srv, err := relay.NewServer(relay.ServerConfig{
		PublicIP: "127.0.0.1",
		BindAddr: "127.0.0.1",
		BindPort: freeUDPPort(t),
		Issuer:   relay.NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).Port
}

func relayGet(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRelayHealthMux_should_keep_healthz_ok_in_every_state(t *testing.T) {
	srv := newTestRelayServer(t)
	rc := &config.RelayConfig{LifecycleToken: relayLifecycleTestToken}
	lc := newRelayLifecycle(rc, srv)
	mux, err := relayHealthMux(rc, srv, lc)
	require.NoError(t, err)

	steps := []struct {
		name    string
		advance func()
	}{
		{"starting", func() {}},
		{"active", lc.MarkReady},
		{"draining", lc.Drain},
		{"stopping", func() { lc.Shutdown(time.Millisecond) }},
	}
	for _, step := range steps {
		step.advance()

		rec := relayGet(t, mux, "/healthz", "")

		assert.Equal(t, http.StatusOK, rec.Code, "healthz in %s", step.name)
	}
}

func TestRelayHealthMux_should_report_relay_details(t *testing.T) {
	srv := newTestRelayServer(t)
	rc := &config.RelayConfig{LifecycleToken: relayLifecycleTestToken}
	lc := newRelayLifecycle(rc, srv)
	mux, err := relayHealthMux(rc, srv, lc)
	require.NoError(t, err)
	lc.MarkReady()

	rec := relayGet(t, mux, "/lifecycle", relayLifecycleTestToken)

	var got struct {
		Role    string         `json:"role"`
		Details map[string]any `json:"details"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "relay", got.Role)
	assert.Equal(t, map[string]any{
		"active_allocations": float64(0),
		"public_ip":          "127.0.0.1",
		"transports":         []any{"udp"},
	}, got.Details)
}

func TestRelayHealthMux_should_expose_lifecycle_state_metric(t *testing.T) {
	srv := newTestRelayServer(t)
	rc := &config.RelayConfig{}
	lc := newRelayLifecycle(rc, srv)
	mux, err := relayHealthMux(rc, srv, lc)
	require.NoError(t, err)
	lc.MarkReady()

	body, err := io.ReadAll(relayGet(t, mux, "/metrics", "").Body)
	require.NoError(t, err)

	assert.Contains(t, string(body), `ggscale_lifecycle_state{state="active"} 1`)
}

func TestRelayHealthMux_should_drain_through_post_route(t *testing.T) {
	srv := newTestRelayServer(t)
	rc := &config.RelayConfig{LifecycleToken: relayLifecycleTestToken}
	lc := newRelayLifecycle(rc, srv)
	mux, err := relayHealthMux(rc, srv, lc)
	require.NoError(t, err)
	lc.MarkReady()
	req := httptest.NewRequest(http.MethodPost, "/lifecycle/drain", nil)
	req.Header.Set("Authorization", "Bearer "+relayLifecycleTestToken)

	mux.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, http.StatusServiceUnavailable, relayGet(t, mux, "/readyz", "").Code)
}

func TestRelayHealthMux_should_accept_metrics_token_with_lowercase_scheme(t *testing.T) {
	srv := newTestRelayServer(t)
	rc := &config.RelayConfig{MetricsToken: relayLifecycleTestToken}
	mux, err := relayHealthMux(rc, srv, newRelayLifecycle(rc, srv))
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "bearer "+relayLifecycleTestToken)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRelayHealthMux_should_refuse_metrics_without_token(t *testing.T) {
	srv := newTestRelayServer(t)
	rc := &config.RelayConfig{MetricsToken: relayLifecycleTestToken}
	mux, err := relayHealthMux(rc, srv, newRelayLifecycle(rc, srv))
	require.NoError(t, err)

	rec := relayGet(t, mux, "/metrics", "")

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
