//go:build integration

package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/realtime"
	"github.com/ggscale/ggscale/internal/tenant"
)

// newRealtimeServer builds a full router with the WS hub enabled and a fast
// heartbeat so lifecycle revalidation fires within the test's patience.
func newRealtimeServer(t *testing.T, c *cluster) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	router := httpapi.NewRouter(httpapi.Deps{
		Version: "v1", Commit: "test",
		Pool:              pool,
		Lookup:            tenant.NewSQLLookup(c.appPool),
		Limiter:           ratelimit.NewCacheLimiter(c.cache),
		Signer:            signer,
		Cache:             c.cache,
		RBAC:              authorizer,
		Hub:               realtime.NewHub(),
		RealtimeHeartbeat: 100 * time.Millisecond,
	})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv
}

func dialRealtime(t *testing.T, ctx context.Context, srvURL, apiKey, sessionToken string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/v1/ws"
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization":   {"Bearer " + apiKey},
		"X-Session-Token": {sessionToken},
	}})
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	require.NoError(t, err)
	return conn
}

// expectSocketClose fails unless the connection's read loop ends within the
// deadline (the server closed the socket).
func expectSocketClose(t *testing.T, ctx context.Context, conn *websocket.Conn, msg string) {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err := conn.Read(readCtx)
	require.Error(t, err, msg)
	require.NotErrorIs(t, err, context.DeadlineExceeded, msg)
}

func TestRealtime_open_socket_closes_when_tenant_disabled(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ws-dis")
	srv := newRealtimeServer(t, c)

	tok, _ := anonymousLoginWithID(t, srv.URL, "key-ws-dis")
	conn := dialRealtime(t, ctx, srv.URL, "key-ws-dis", tok)
	defer conn.CloseNow() //nolint:errcheck // best-effort cleanup

	// Disable the tenant AFTER the handshake: the heartbeat revalidation
	// must terminate the established socket.
	_, err := c.bootstrapPool.Exec(ctx,
		`UPDATE tenants SET disabled_at = now(), disabled_by = 'platform' WHERE id = $1`, tenantID)
	require.NoError(t, err)

	expectSocketClose(t, ctx, conn, "an open socket must close after tenant disable")
}

func TestRealtime_open_socket_closes_when_session_epoch_bumps(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ws-epoch")
	srv := newRealtimeServer(t, c)

	tok, playerID := anonymousLoginWithID(t, srv.URL, "key-ws-epoch")
	conn := dialRealtime(t, ctx, srv.URL, "key-ws-epoch", tok)
	defer conn.CloseNow() //nolint:errcheck // best-effort cleanup

	// An epoch bump is what unlink / ban / disable / password change do.
	_, err := c.bootstrapPool.Exec(ctx,
		`UPDATE project_players SET session_epoch = session_epoch + 1 WHERE id = $1`, playerID)
	require.NoError(t, err)

	expectSocketClose(t, ctx, conn, "an open socket must close after a session-epoch bump")
}
