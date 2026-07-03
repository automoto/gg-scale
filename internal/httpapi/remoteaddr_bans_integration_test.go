//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoteAddr_owner_and_friend_acl exercises the remote-address ACL:
// owner read/write, accepted-friend read, non-friend denial, and the
// unlinked-player 403.
func TestRemoteAddr_owner_and_friend_acl(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, idA)
	linkPlayerAccount(t, c, idB)

	// Owner writes and reads back.
	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tokA,
		map[string]any{"primary_remote_addr": "100.64.0.1:9000"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/account/remote-addrs", "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var got struct {
		Primary *string `json:"primary_remote_addr"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotNil(t, got.Primary)
	assert.Equal(t, "100.64.0.1:9000", *got.Primary)

	// Non-friend B cannot read A's address.
	resp, _ = authedReq(t, http.MethodGet, fmt.Sprintf("%s/v1/friends/%d/remote-addrs", srv.URL, idA), "k", tokB, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// After they become friends, B can read A's address.
	makeFriends(t, c, srv.URL, "k", idA, tokA, idB, tokB)
	resp, body = authedReq(t, http.MethodGet, fmt.Sprintf("%s/v1/friends/%d/remote-addrs", srv.URL, idA), "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotNil(t, got.Primary)
	assert.Equal(t, "100.64.0.1:9000", *got.Primary)

	// An unlinked (anonymous) player is told to link an account.
	tokC, _ := anonymousLoginWithID(t, srv.URL, "k")
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/account/remote-addrs", "k", tokC, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "link a gg-scale account")
}

// TestRemoteAddr_rejects_control_chars proves the printable-non-control
// validation.
func TestRemoteAddr_rejects_control_chars(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)
	tok, id := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, id)

	resp, _ := authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tok,
		map[string]any{"primary_remote_addr": "bad\x00addr"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestTenantBan_blocks_login is the tenant-ban enforcement at the login point.
func TestTenantBan_blocks_login(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	// Sign up an email/password player, then link + ban their account.
	resp, body := doJSON(t, http.MethodPost, srv.URL+"/v1/auth/signup", "k",
		map[string]string{"email": "banme@example.com", "password": "hunter2hunter2"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode, string(body))

	var playerID int64
	var tenantID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT id, tenant_id FROM project_players WHERE email = 'banme@example.com'`).Scan(&playerID, &tenantID))
	accID := linkPlayerAccountReturningID(t, c, playerID)

	// Login works before the ban.
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "banme@example.com", "password": "hunter2hunter2"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// Ban the account tenant-wide (and bump epoch as the dashboard handler does).
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO tenant_player_bans (tenant_id, player_account_id) VALUES ($1, $2)`, tenantID, accID)
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET session_epoch = session_epoch + 1 WHERE player_account_id = $1 AND tenant_id = $2`, accID, tenantID)
	require.NoError(t, err)

	// Login is now refused.
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/v1/auth/login", "k",
		map[string]string{"email": "banme@example.com", "password": "hunter2hunter2"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}

func linkPlayerAccountReturningID(t *testing.T, c *cluster, playerID int64) string {
	t.Helper()
	var accID string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO player_accounts (email, password_hash, email_verified_at)
		 VALUES ($1, '\x00'::bytea, now()) RETURNING id`,
		fmt.Sprintf("acct-%d@example.com", playerID)).Scan(&accID))
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET player_account_id = $1 WHERE id = $2`, accID, playerID)
	require.NoError(t, err)
	return accID
}
