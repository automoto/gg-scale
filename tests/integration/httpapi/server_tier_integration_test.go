//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Server-tier routes: a dedicated game server holds a secret API key and no
// player session. It submits scores and reads/writes storage FOR a player it
// names by id. Publishable keys (embedded in shipped game binaries) must be
// rejected on every /v1/server/ route.

func seedLeaderboard(t *testing.T, c *cluster, tenantID, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, $3) RETURNING id`,
		tenantID, projectID, name).Scan(&id))
	return id
}

func disablePlayerRow(t *testing.T, c *cluster, playerID int64) {
	t.Helper()
	_, err := c.bootstrapPool.Exec(context.Background(),
		`UPDATE project_players SET disabled_at = now() WHERE id = $1`, playerID)
	require.NoError(t, err)
}

func banPlayer(t *testing.T, c *cluster, tenantID, playerID int64) {
	t.Helper()
	accID := linkPlayerAccountReturningID(t, c, playerID)
	_, err := c.bootstrapPool.Exec(context.Background(),
		`INSERT INTO tenant_player_bans (tenant_id, player_account_id, reason) VALUES ($1, $2, 'test')`,
		tenantID, accID)
	require.NoError(t, err)
}

func serverScoresURL(srvURL string, boardID int64) string {
	return fmt.Sprintf("%s/v1/server/leaderboards/%d/scores", srvURL, boardID)
}

func serverStorageURL(srvURL string, playerID int64, key string) string {
	base := fmt.Sprintf("%s/v1/server/players/%d/storage/objects", srvURL, playerID)
	if key == "" {
		return base
	}
	return base + "/" + key
}

func scoreBody(t *testing.T, playerID, score int64) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]int64{"player_id": playerID, "score": score})
	require.NoError(t, err)
	return b
}

func countScores(t *testing.T, c *cluster, boardID, playerID int64) int {
	t.Helper()
	var n int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM leaderboard_entries WHERE leaderboard_id = $1 AND player_id = $2`,
		boardID, playerID).Scan(&n))
	return n
}

func TestServerTier_submit_score_creates_entry(t *testing.T) {
	// Arrange
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "srv-score")
	playerID := insertPlayer(t, c, tenantID, projectID, "srv-score-p1")
	boardID := seedLeaderboard(t, c, tenantID, projectID, "board")
	srv := newServerForCluster(t, c)

	// Act
	res := storageRequestStatus(http.MethodPost, serverScoresURL(srv.URL, boardID),
		"srv-score", "", scoreBody(t, playerID, 1500), "")

	// Assert
	require.NoError(t, res.err)
	assert.Equal(t, http.StatusCreated, res.status, res.body)
	assert.Equal(t, 1, countScores(t, c, boardID, playerID))
}

func TestServerTier_submit_score_rejections(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "srv-score-neg")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "srv-score-pub", "publishable")
	otherTenant, otherProject := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "srv-score-other")

	boardID := seedLeaderboard(t, c, tenantID, projectID, "board")
	siblingBoard := seedLeaderboard(t, c, otherTenant, otherProject, "sibling")

	okPlayer := insertPlayer(t, c, tenantID, projectID, "srv-neg-ok")
	siblingPlayer := insertPlayer(t, c, otherTenant, otherProject, "srv-neg-sibling")
	deletedPlayer := insertPlayer(t, c, tenantID, projectID, "srv-neg-del")
	deletePlayer(t, c, deletedPlayer)
	disabledPlayer := insertPlayer(t, c, tenantID, projectID, "srv-neg-dis")
	disablePlayerRow(t, c, disabledPlayer)
	bannedPlayer := insertPlayer(t, c, tenantID, projectID, "srv-neg-ban")
	banPlayer(t, c, tenantID, bannedPlayer)

	srv := newServerForCluster(t, c)

	cases := []struct {
		name       string
		apiKey     string
		boardID    int64
		playerID   int64
		wantStatus int
	}{
		{"publishable_key_is_rejected", "srv-score-pub", boardID, okPlayer, http.StatusForbidden},
		{"missing_key_is_rejected", "", boardID, okPlayer, http.StatusUnauthorized},
		{"sibling_project_player_is_not_found", "srv-score-neg", boardID, siblingPlayer, http.StatusNotFound},
		{"unknown_player_is_not_found", "srv-score-neg", boardID, 999999999, http.StatusNotFound},
		{"deleted_player_is_not_found", "srv-score-neg", boardID, deletedPlayer, http.StatusNotFound},
		{"disabled_player_is_forbidden", "srv-score-neg", boardID, disabledPlayer, http.StatusForbidden},
		{"banned_player_is_forbidden", "srv-score-neg", boardID, bannedPlayer, http.StatusForbidden},
		{"sibling_project_board_is_not_found", "srv-score-neg", siblingBoard, okPlayer, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := storageRequestStatus(http.MethodPost, serverScoresURL(srv.URL, tc.boardID),
				tc.apiKey, "", scoreBody(t, tc.playerID, 100), "")
			require.NoError(t, res.err)
			assert.Equal(t, tc.wantStatus, res.status, res.body)
		})
	}

	var total int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM leaderboard_entries WHERE leaderboard_id = $1 OR leaderboard_id = $2`,
		boardID, siblingBoard).Scan(&total))
	assert.Zero(t, total, "no rejection may write a score")
}

func TestServerTier_storage_roundtrip_visible_to_player_session(t *testing.T) {
	// Arrange
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "srv-store")
	playerID := insertPlayer(t, c, tenantID, projectID, "srv-store-p1")
	srv := newServerForCluster(t, c)

	// Act: server-tier put, get, list.
	put := storageRequestStatus(http.MethodPut, serverStorageURL(srv.URL, playerID, "slot-1"),
		"srv-store", "", []byte(`{"level":3}`), "")
	get := storageRequestStatus(http.MethodGet, serverStorageURL(srv.URL, playerID, "slot-1"),
		"srv-store", "", nil, "")
	list := storageRequestStatus(http.MethodGet, serverStorageURL(srv.URL, playerID, ""),
		"srv-store", "", nil, "")

	// Assert
	require.NoError(t, put.err)
	require.Equal(t, http.StatusOK, put.status, put.body)
	var putObj struct {
		Key     string          `json:"key"`
		Value   json.RawMessage `json:"value"`
		Version int64           `json:"version"`
	}
	require.NoError(t, json.Unmarshal([]byte(put.body), &putObj))
	assert.Equal(t, "slot-1", putObj.Key)
	assert.Equal(t, int64(1), putObj.Version)

	require.NoError(t, get.err)
	require.Equal(t, http.StatusOK, get.status, get.body)
	assert.Contains(t, get.body, `"level":3`)

	require.NoError(t, list.err)
	require.Equal(t, http.StatusOK, list.status, list.body)
	assert.Contains(t, list.body, `"slot-1"`)

	// The object the server wrote is the same one the player session reads.
	access := signSession(t, newTestSigner(t), tenantID, projectID, playerID, time.Hour)
	viaPlayer := storageRequestStatus(http.MethodGet, srv.URL+"/v1/storage/objects/slot-1",
		"srv-store", access, nil, "")
	require.NoError(t, viaPlayer.err)
	require.Equal(t, http.StatusOK, viaPlayer.status, viaPlayer.body)
	assert.Contains(t, viaPlayer.body, `"level":3`)

	assertTenantStorageUsageMatches(t, c, tenantID)
}

func TestServerTier_storage_put_honors_if_match(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "srv-ifmatch")
	playerID := insertPlayer(t, c, tenantID, projectID, "srv-ifmatch-p1")
	srv := newServerForCluster(t, c)
	target := serverStorageURL(srv.URL, playerID, "slot-1")

	first := storageRequestStatus(http.MethodPut, target, "srv-ifmatch", "", []byte(`{"v":1}`), "")
	require.NoError(t, first.err)
	require.Equal(t, http.StatusOK, first.status, first.body)

	stale := storageRequestStatus(http.MethodPut, target, "srv-ifmatch", "", []byte(`{"v":2}`), "42")
	require.NoError(t, stale.err)
	assert.Equal(t, http.StatusPreconditionFailed, stale.status, stale.body)

	current := storageRequestStatus(http.MethodPut, target, "srv-ifmatch", "", []byte(`{"v":2}`), "1")
	require.NoError(t, current.err)
	require.Equal(t, http.StatusOK, current.status, current.body)
	var obj struct {
		Version int64 `json:"version"`
	}
	require.NoError(t, json.Unmarshal([]byte(current.body), &obj))
	assert.Equal(t, int64(2), obj.Version)
}

func TestServerTier_storage_rejections(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "srv-store-neg")
	seedAPIKey(t, c.bootstrapPool, tenantID, &projectID, "srv-store-pub", "publishable")
	otherTenant, otherProject := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "srv-store-other")

	okPlayer := insertPlayer(t, c, tenantID, projectID, "srv-sneg-ok")
	siblingPlayer := insertPlayer(t, c, otherTenant, otherProject, "srv-sneg-sib")
	disabledPlayer := insertPlayer(t, c, tenantID, projectID, "srv-sneg-dis")
	disablePlayerRow(t, c, disabledPlayer)

	srv := newServerForCluster(t, c)

	cases := []struct {
		name       string
		method     string
		target     string
		apiKey     string
		body       []byte
		wantStatus int
	}{
		{"publishable_put", http.MethodPut, serverStorageURL(srv.URL, okPlayer, "k"), "srv-store-pub", []byte(`{}`), http.StatusForbidden},
		{"publishable_get", http.MethodGet, serverStorageURL(srv.URL, okPlayer, "k"), "srv-store-pub", nil, http.StatusForbidden},
		{"publishable_list", http.MethodGet, serverStorageURL(srv.URL, okPlayer, ""), "srv-store-pub", nil, http.StatusForbidden},
		{"missing_key", http.MethodPut, serverStorageURL(srv.URL, okPlayer, "k"), "", []byte(`{}`), http.StatusUnauthorized},
		{"sibling_project_player_put", http.MethodPut, serverStorageURL(srv.URL, siblingPlayer, "k"), "srv-store-neg", []byte(`{}`), http.StatusNotFound},
		{"sibling_project_player_get", http.MethodGet, serverStorageURL(srv.URL, siblingPlayer, "k"), "srv-store-neg", nil, http.StatusNotFound},
		{"sibling_project_player_list", http.MethodGet, serverStorageURL(srv.URL, siblingPlayer, ""), "srv-store-neg", nil, http.StatusNotFound},
		{"unknown_player_put", http.MethodPut, serverStorageURL(srv.URL, 999999999, "k"), "srv-store-neg", []byte(`{}`), http.StatusNotFound},
		{"disabled_player_put", http.MethodPut, serverStorageURL(srv.URL, disabledPlayer, "k"), "srv-store-neg", []byte(`{}`), http.StatusForbidden},
		{"unknown_object_get", http.MethodGet, serverStorageURL(srv.URL, okPlayer, "absent"), "srv-store-neg", nil, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := storageRequestStatus(tc.method, tc.target, tc.apiKey, "", tc.body, "")
			require.NoError(t, res.err)
			assert.Equal(t, tc.wantStatus, res.status, res.body)
		})
	}

	var count int
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT count(*) FROM storage_objects WHERE tenant_id = $1 OR tenant_id = $2`,
		tenantID, otherTenant).Scan(&count))
	assert.Zero(t, count, "no rejection may write an object")
}
