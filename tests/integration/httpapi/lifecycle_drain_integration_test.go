//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/lifecycle"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/realtime"
	"github.com/automoto/gg-scale/internal/tenant"
)

const drainIntegrationToken = "drain-integration-token-0123456789abcdef"

type drainServer struct {
	url string
	hub *realtime.Hub
	lc  *lifecycle.Controller
}

// newDrainServer builds the full router the way main does: lifecycle routes
// at root, the realtime drain on /v1/ws, and the in-flight counter in front.
func newDrainServer(t *testing.T, c *cluster) drainServer {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	hub := realtime.NewHub()
	work := httpapi.NewAPIWork()
	lc := lifecycle.New(lifecycle.Config{Role: "api", Token: drainIntegrationToken, Work: work, Details: work.Details})
	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1", Commit: "test",
		Pool:              pool,
		Lookup:            tenant.NewSQLLookup(c.appPool),
		Limiter:           ratelimit.NewCacheLimiter(c.cache),
		Signer:            signer,
		Cache:             c.cache,
		RBAC:              authorizer,
		Hub:               hub,
		RealtimeHeartbeat: time.Hour,
		Lifecycle:         lc,
		RealtimeDrain:     work.Realtime,
	})
	srv := httptest.NewServer(work.Inflight.Middleware(router))
	t.Cleanup(srv.Close)
	lc.MarkReady()
	return drainServer{url: srv.URL, hub: hub, lc: lc}
}

func (s drainServer) post(t *testing.T, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.url+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+drainIntegrationToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

// dialStatus opens /v1/ws with API key and session headers and returns the
// handshake status.
func dialStatus(t *testing.T, srvURL, apiKey, sessionToken string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/v1/ws"
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization":   {"Bearer " + apiKey},
		"X-Session-Token": {sessionToken},
	}})
	if conn != nil {
		_ = conn.CloseNow()
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err == nil {
		return http.StatusSwitchingProtocols
	}
	require.NotNil(t, resp, "dial error without a response: %v", err)
	return resp.StatusCode
}

func TestLifecycleDrain_refuses_new_websocket_upgrade_with_503(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-drain-refuse")
	srv := newDrainServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.url, "key-drain-refuse")
	require.Equal(t, http.StatusOK, srv.post(t, "/lifecycle/drain"))

	status := dialStatus(t, srv.url, "key-drain-refuse", tok)

	assert.Equal(t, http.StatusServiceUnavailable, status)
}

func TestLifecycleDrain_keeps_ticket_usable_after_refused_upgrade(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-drain-ticket")
	srv := newDrainServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.url, "key-drain-ticket")
	ticket := realtimeTicket(t, srv.url, "key-drain-ticket", tok)
	srv.post(t, "/lifecycle/drain")
	require.Equal(t, http.StatusServiceUnavailable, dialWithTicket(t, srv.url, ticket))
	srv.post(t, "/lifecycle/resume")

	status := dialWithTicket(t, srv.url, ticket)

	assert.Equal(t, http.StatusSwitchingProtocols, status)
}

func TestLifecycleDrain_keeps_existing_websocket_receiving(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-drain-keep")
	srv := newDrainServer(t, c)
	tok, playerID := anonymousLoginWithID(t, srv.url, "key-drain-keep")
	conn := dialRealtime(t, ctx, srv.url, "key-drain-keep", tok)
	defer conn.CloseNow() //nolint:errcheck // best-effort cleanup
	require.Eventually(t, func() bool { return srv.hub.Count() == 1 }, 2*time.Second, 10*time.Millisecond)
	srv.post(t, "/lifecycle/drain")

	require.NoError(t, srv.hub.Send(ctx, tenantID, playerID, realtime.Message{Type: "still_here"}))

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"still_here"}`, string(data))
}

func TestLifecycleDrain_keeps_serving_plain_healthz(t *testing.T) {
	c := startCluster(t)
	srv := newDrainServer(t, c)
	srv.post(t, "/lifecycle/drain")

	resp, err := http.Get(srv.url + "/v1/healthz")
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestLifecycleDrain_accepts_websocket_upgrades_after_resume(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-drain-resume")
	srv := newDrainServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.url, "key-drain-resume")
	srv.post(t, "/lifecycle/drain")
	require.Equal(t, http.StatusOK, srv.post(t, "/lifecycle/resume"))

	status := dialStatus(t, srv.url, "key-drain-resume", tok)

	assert.Equal(t, http.StatusSwitchingProtocols, status)
}
