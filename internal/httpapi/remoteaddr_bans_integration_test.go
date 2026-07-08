//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type remoteAddrEntry struct {
	Type    string `json:"type"`
	Scope   string `json:"scope,omitempty"`
	Address string `json:"address"`
}

type remoteAddrsBody struct {
	Addresses []remoteAddrEntry `json:"addresses"`
}

// TestRemoteAddr_owner_and_friend_acl exercises the remote-address ACL:
// owner read/write, accepted-friend read, non-friend denial, and the
// unlinked-player 403 — plus server-side scope detection.
func TestRemoteAddr_owner_and_friend_acl(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, idA)
	linkPlayerAccount(t, c, idB)

	// Owner writes a LAN + a public IP; scopes are detected, slot order fixed.
	want := []remoteAddrEntry{
		{Type: "ip", Scope: "lan", Address: "192.168.1.4:9000"},
		{Type: "ip", Scope: "public", Address: "203.0.113.9:9000"},
	}
	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tokA,
		remoteAddrsBody{Addresses: []remoteAddrEntry{
			{Type: "ip", Address: "203.0.113.9:9000"},
			{Type: "ip", Address: "192.168.1.4:9000"},
		}})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var got remoteAddrsBody
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, want, got.Addresses)

	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/account/remote-addrs", "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, want, got.Addresses)

	// Non-friend B cannot read A's address.
	resp, _ = authedReq(t, http.MethodGet, fmt.Sprintf("%s/v1/friends/%d/remote-addrs", srv.URL, idA), "k", tokB, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// After they become friends, B can read A's address.
	makeFriends(t, c, srv.URL, "k", idA, tokA, idB, tokB)
	resp, body = authedReq(t, http.MethodGet, fmt.Sprintf("%s/v1/friends/%d/remote-addrs", srv.URL, idA), "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, want, got.Addresses)

	// An unlinked (anonymous) player is told to link an account.
	tokC, _ := anonymousLoginWithID(t, srv.URL, "k")
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/account/remote-addrs", "k", tokC, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "link a gg-scale account")
}

// TestRemoteAddr_rejects_invalid_and_duplicate_slots proves per-type
// validation and the one-address-per-slot rule.
func TestRemoteAddr_rejects_invalid_and_duplicate_slots(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)
	tok, id := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, id)

	put := func(entries ...remoteAddrEntry) (*http.Response, []byte) {
		return authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tok,
			remoteAddrsBody{Addresses: entries})
	}

	resp, body := put(remoteAddrEntry{Type: "iroh", Address: "not-hex"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "addresses[0]")

	resp, body = put(remoteAddrEntry{Type: "dns", Address: "1.2.3.4"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "IP address type")

	resp, body = put(remoteAddrEntry{Type: "tailscale", Address: "whatever"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "unknown")

	resp, body = put(
		remoteAddrEntry{Type: "ip", Address: "203.0.113.9"},
		remoteAddrEntry{Type: "ip", Address: "198.51.100.7"})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "public IP address is already set")

	many := make([]remoteAddrEntry, 5)
	for i := range many {
		many[i] = remoteAddrEntry{Type: "dns", Address: "example.com"}
	}
	resp, body = put(many...)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "too many addresses")
}

// TestRemoteAddr_put_is_full_replace proves PUT semantics: the submitted
// list becomes the account's complete address set.
func TestRemoteAddr_put_is_full_replace(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)
	tok, id := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, id)

	iroh := strings.Repeat("ab", 32)
	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tok,
		remoteAddrsBody{Addresses: []remoteAddrEntry{
			{Type: "ip", Address: "192.168.1.4"},
			{Type: "dns", Address: "example.com:7777"},
		}})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tok,
		remoteAddrsBody{Addresses: []remoteAddrEntry{{Type: "iroh", Address: iroh}}})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var got remoteAddrsBody
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, []remoteAddrEntry{{Type: "iroh", Address: iroh}}, got.Addresses)
}

// TestRemoteAddr_get_scope_field_roundtrips_into_put guards the
// DisallowUnknownFields trap: PUTting a GET body verbatim (including the
// server-derived scope field) must succeed.
func TestRemoteAddr_get_scope_field_roundtrips_into_put(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)
	tok, id := anonymousLoginWithID(t, srv.URL, "k")
	linkPlayerAccount(t, c, id)

	resp, body := authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tok,
		remoteAddrsBody{Addresses: []remoteAddrEntry{{Type: "ip", Address: "192.168.1.4"}}})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	resp, body = authedReq(t, http.MethodPut, srv.URL+"/v1/account/remote-addrs", "k", tok, raw)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
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

	// Ban the account tenant-wide (and bump epoch as the control panel handler does).
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
