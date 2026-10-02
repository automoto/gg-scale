//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func realtimeTicket(t *testing.T, srvURL, apiKey, sessionToken string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/ws/ticket", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Session-Token", sessionToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var body struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotEmpty(t, body.Ticket)
	return body.Ticket
}

// dialWithTicket opens /v1/ws the way a browser does: no auth headers, the
// ticket in the URL. It returns the HTTP status of the handshake.
func dialWithTicket(t *testing.T, srvURL, ticket string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/v1/ws?ticket=" + url.QueryEscape(ticket)
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
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

func TestRealtimeTicket_opens_a_socket_without_headers(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ticket-ok")
	srv := newRealtimeServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "key-ticket-ok")

	status := dialWithTicket(t, srv.URL, realtimeTicket(t, srv.URL, "key-ticket-ok", tok))

	assert.Equal(t, http.StatusSwitchingProtocols, status)
}

func TestRealtimeTicket_works_only_once(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ticket-once")
	srv := newRealtimeServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "key-ticket-once")
	ticket := realtimeTicket(t, srv.URL, "key-ticket-once", tok)
	require.Equal(t, http.StatusSwitchingProtocols, dialWithTicket(t, srv.URL, ticket))

	status := dialWithTicket(t, srv.URL, ticket)

	assert.Equal(t, http.StatusUnauthorized, status)
}

func TestRealtimeTicket_refuses_unknown_and_expired_tickets(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ticket-exp")
	srv := newRealtimeServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "key-ticket-exp")
	expired := realtimeTicket(t, srv.URL, "key-ticket-exp", tok)
	_, err := c.bootstrapPool.Exec(context.Background(), `UPDATE realtime_tickets SET expires_at = now() - interval '1 second'`)
	require.NoError(t, err)

	got := []int{dialWithTicket(t, srv.URL, "not-a-ticket"), dialWithTicket(t, srv.URL, expired)}

	assert.Equal(t, []int{http.StatusUnauthorized, http.StatusUnauthorized}, got)
}

func TestRealtimeTicket_is_refused_after_key_revoke(t *testing.T) {
	c := startCluster(t)
	tenantID, _ := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ticket-rev")
	srv := newRealtimeServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "key-ticket-rev")
	ticket := realtimeTicket(t, srv.URL, "key-ticket-rev", tok)
	_, err := c.bootstrapPool.Exec(context.Background(), `UPDATE api_keys SET revoked_at = now() WHERE tenant_id = $1`, tenantID)
	require.NoError(t, err)

	status := dialWithTicket(t, srv.URL, ticket)

	assert.Equal(t, http.StatusForbidden, status)
}

func TestRealtimeTicket_is_refused_after_session_epoch_bump(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ticket-epoch")
	srv := newRealtimeServer(t, c)
	tok, playerID := anonymousLoginWithID(t, srv.URL, "key-ticket-epoch")
	ticket := realtimeTicket(t, srv.URL, "key-ticket-epoch", tok)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET session_epoch = session_epoch + 1 WHERE id = $1`, playerID)
	require.NoError(t, err)

	status := dialWithTicket(t, srv.URL, ticket)

	assert.Equal(t, http.StatusUnauthorized, status)
}

func TestRealtimeTicket_requires_player_session(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ticket-nosess")
	srv := newRealtimeServer(t, c)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ws/ticket", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer key-ticket-nosess")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestRealtime_header_path_still_works(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-ws-headers")
	srv := newRealtimeServer(t, c)
	tok, _ := anonymousLoginWithID(t, srv.URL, "key-ws-headers")

	conn := dialRealtime(t, ctx, srv.URL, "key-ws-headers", tok)

	assert.NoError(t, conn.Close(websocket.StatusNormalClosure, ""))
}
