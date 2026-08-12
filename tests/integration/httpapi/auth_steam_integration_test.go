//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/secretseal"
	"github.com/automoto/gg-scale/internal/steamauth"
	"github.com/automoto/gg-scale/internal/tenant"
)

const (
	steamTestKey    = "0123456789ABCDEF0123456789ABCDEF"
	steamTestID     = "76561197960265728"
	steamTestTicket = "14000000048bcd42aabbccdd"
)

// fakeValveServer emits a Valve-shaped response and counts requests.
func fakeValveServer(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func valveOK(steamID string, vac, publisher bool) string {
	vacStr, pubStr := "false", "false"
	if vac {
		vacStr = "true"
	}
	if publisher {
		pubStr = "true"
	}
	return `{"response":{"params":{"result":"OK","steamid":"` + steamID +
		`","ownersteamid":"76561197960265999","vacbanned":` + vacStr +
		`,"publisherbanned":` + pubStr + `}}}`
}

// newServerWithSteamVerifier builds the API server with the real steamauth
// client pointed at a fake Valve, plus the credential cipher so sealed keys
// unseal on read.
func newServerWithSteamVerifier(t *testing.T, c *cluster, valveURL string) (*httptest.Server, *secretseal.Cipher) {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	cipher, err := secretseal.Load(context.Background(), pool, "")
	require.NoError(t, err)

	h := httpapi.NewRouter(httpapi.Deps{
		Version:          "v1",
		Commit:           "test",
		Pool:             pool,
		Lookup:           tenant.NewSQLLookup(c.appPool),
		Limiter:          ratelimit.NewCacheLimiter(c.cache),
		Signer:           signer,
		Cache:            c.cache,
		RBAC:             authorizer,
		CredentialCipher: cipher,
		SteamAuth:        &steamauth.Client{BaseURL: valveURL},
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, cipher
}

// seedSteamConfig stores the project's Steam credentials, sealed like the
// control panel writes them.
func seedSteamConfig(t *testing.T, c *cluster, cipher *secretseal.Cipher, projectID int64, appID, key string) {
	t.Helper()
	sealed, err := cipher.Encrypt([]byte(key))
	require.NoError(t, err)
	_, err = c.bootstrapPool.Exec(context.Background(),
		`UPDATE projects SET steam_app_id = $1, steam_web_api_key = $2 WHERE id = $3`,
		appID, sealed, projectID)
	require.NoError(t, err)
}

func postSteamAuth(t *testing.T, baseURL, apiKey, ticket string) (*http.Response, []byte) {
	t.Helper()
	return doJSON(t, http.MethodPost, baseURL+"/v1/auth/steam", apiKey,
		map[string]string{"ticket": ticket})
}

type steamSessionBody struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	PlayerID     int64  `json:"player_id"`
}

func TestAuthSteam_mints_session_and_creates_player(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "st")
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	resp, body := postSteamAuth(t, srv.URL, "st", steamTestTicket)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var out steamSessionBody
	require.NoError(t, json.Unmarshal(body, &out))
	assert.NotEmpty(t, out.AccessToken)
	assert.NotEmpty(t, out.RefreshToken)
	require.Positive(t, out.PlayerID)

	var externalID string
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT external_id FROM project_players WHERE id = $1`, out.PlayerID).Scan(&externalID))
	assert.Equal(t, "steam:"+steamTestID, externalID)
}

func TestAuthSteam_repeat_sign_in_maps_to_same_player(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "st")
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	resp, body := postSteamAuth(t, srv.URL, "st", steamTestTicket)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var first steamSessionBody
	require.NoError(t, json.Unmarshal(body, &first))

	resp, body = postSteamAuth(t, srv.URL, "st", steamTestTicket)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var second steamSessionBody
	require.NoError(t, json.Unmarshal(body, &second))

	assert.Equal(t, first.PlayerID, second.PlayerID)
	var n int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM project_players WHERE external_id = $1`,
		"steam:"+steamTestID).Scan(&n))
	assert.Equal(t, int64(1), n)
}

func TestAuthSteam_invalid_ticket_is_401(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "st")
	valve, _ := fakeValveServer(t,
		`{"response":{"error":{"errorcode":101,"errordesc":"Invalid ticket"}}}`)
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	resp, body := postSteamAuth(t, srv.URL, "st", steamTestTicket)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
}

func TestAuthSteam_banned_accounts_are_403(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"vac_banned", valveOK(steamTestID, true, false)},
		{"publisher_banned", valveOK(steamTestID, false, true)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := startCluster(t)
			_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "st")
			valve, _ := fakeValveServer(t, tt.body)
			srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
			seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

			resp, body := postSteamAuth(t, srv.URL, "st", steamTestTicket)
			assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
			var n int64
			require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
				`SELECT count(*) FROM project_players WHERE external_id = $1`,
				"steam:"+steamTestID).Scan(&n))
			assert.Zero(t, n, "a banned sign-in must not create a player")
		})
	}
}

func TestAuthSteam_unconfigured_is_400_and_never_calls_valve(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "st")
	valve, calls := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, _ := newServerWithSteamVerifier(t, c, valve.URL)

	resp, body := postSteamAuth(t, srv.URL, "st", steamTestTicket)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
	assert.Contains(t, string(body), "not configured")
	assert.Zero(t, calls.Load(), "an unconfigured project must not reach Valve")
}

func TestAuthSteam_valve_outage_is_502(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "st")
	valve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(valve.Close)
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	resp, body := postSteamAuth(t, srv.URL, "st", steamTestTicket)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode, string(body))
}

func TestAuthSteam_config_is_per_project(t *testing.T) {
	c := startCluster(t)
	_, projectA := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "sta")
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "stb")
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectA, "480", steamTestKey)

	// Project B's key is pinned to a project with no Steam config.
	resp, body := postSteamAuth(t, srv.URL, "stb", steamTestTicket)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(body))
}

func TestAuthSteam_tenant_banned_player_is_403(t *testing.T) {
	c := startCluster(t)
	_, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "st")
	valve, _ := fakeValveServer(t, valveOK(steamTestID, false, false))
	srv, cipher := newServerWithSteamVerifier(t, c, valve.URL)
	seedSteamConfig(t, c, cipher, projectID, "480", steamTestKey)

	// First sign-in creates the player; the tenant then bans the player's
	// linked account (bans are account-scoped).
	resp, body := postSteamAuth(t, srv.URL, "st", steamTestTicket)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out steamSessionBody
	require.NoError(t, json.Unmarshal(body, &out))
	linkPlayerAccount(t, c, out.PlayerID)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO tenant_player_bans (tenant_id, player_account_id, reason)
		 SELECT p.tenant_id, p.player_account_id, 'cheating'
		 FROM project_players p WHERE p.id = $1`, out.PlayerID)
	require.NoError(t, err)

	resp, body = postSteamAuth(t, srv.URL, "st", steamTestTicket)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))
}
