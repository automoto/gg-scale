//go:build integration

package managementapi_test

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/managementtoken"
)

const (
	queuePath = "/v1/project/matchmaking/queue"
	steamPath = "/v1/project/steam-auth"
)

func (f *fixture) queued(projectID int64, mode, region, gameMode string) {
	f.t.Helper()
	f.queuedWithStatus(projectID, mode, region, gameMode, "queued")
}

func (f *fixture) queuedWithStatus(projectID int64, mode, region, gameMode, status string) {
	f.t.Helper()
	player := f.player(projectID)
	f.exec(`INSERT INTO matchmaking_tickets (tenant_id, project_id, player_id, status, mode, region, game_mode, query)
		SELECT tenant_id, id, $2, $6::ticket_status, $3, $4, $5, '' FROM projects WHERE id = $1`,
		projectID, player, mode, region, gameMode, status)
}

func TestQueue_should_return_403_without_matchmaking_read(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeConfigRead)

	// Act
	got := f.do(tok, http.MethodGet, queuePath, "")

	// Assert
	assert.Equal(t, 403, got.status)
}

func TestQueue_should_show_only_own_project_buckets_and_page_them(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.queued(f.projectA, "match_only", "eu", "duel")
	f.queued(f.projectA, "match_only", "eu", "duel")
	f.queued(f.projectA, "match_only", "us", "duel")
	f.queued(f.projectB, "match_only", "other-project", "duel")
	f.queued(f.projectC, "match_only", "other-tenant", "duel")
	tok := f.scopedToken(managementtoken.ScopeMatchmakingRead)

	// Act
	first := f.do(tok, http.MethodGet, queuePath+"?limit=1", "")
	second := f.do(tok, http.MethodGet, queuePath+"?limit=1&cursor="+first.body["next_cursor"].(string), "")

	// Assert
	b1 := first.body["buckets"].([]any)[0].(map[string]any)
	b2 := second.body["buckets"].([]any)[0].(map[string]any)
	assert.Equal(t, []any{"eu", float64(2), "us", float64(1), nil}, []any{b1["region"], b1["tickets"], b2["region"], b2["tickets"], second.body["next_cursor"]}, first.raw+second.raw)
	assert.NotContains(t, first.raw+second.raw, "player")
}

func TestSteamAuth_should_return_403_without_config_read(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeMatchmakingRead)

	// Act
	got := f.do(tok, http.MethodGet, steamPath, "")

	// Assert
	assert.Equal(t, 403, got.status)
}

func TestSteamAuth_should_report_unset_config(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeConfigRead)

	// Act
	got := f.do(tok, http.MethodGet, steamPath, "")

	// Assert
	assert.Equal(t, []any{200, nil, false}, []any{got.status, got.body["app_id"], got.body["web_api_key_set"]}, got.raw)
}

func TestSteamAuth_should_never_return_the_key(t *testing.T) {
	// Arrange
	f := newFixture(t)
	sealed := []byte{0xde, 0xad, 0xbe, 0xef, 'S', 'E', 'A', 'L', 'E', 'D', 0x01, 0x02, 0xfe}
	f.exec(`UPDATE projects SET steam_app_id = '480', steam_web_api_key = $2 WHERE id = $1`, f.projectA, sealed)
	f.exec(`UPDATE projects SET steam_app_id = '999', steam_web_api_key = 'OTHERKEY'::bytea WHERE id = $1`, f.projectB)
	tok := f.scopedToken(managementtoken.ScopeConfigRead)

	// Act
	got := f.do(tok, http.MethodGet, steamPath, "")

	// Assert
	assert.Equal(t, []any{200, "480", true}, []any{got.status, got.body["app_id"], got.body["web_api_key_set"]}, got.raw)
	for name, form := range map[string]string{
		"raw":       string(sealed),
		"text":      "SEALED",
		"hex":       hex.EncodeToString(sealed),
		"base64":    base64.StdEncoding.EncodeToString(sealed),
		"base64url": base64.RawURLEncoding.EncodeToString(sealed),
	} {
		assert.NotContains(t, got.raw, form, name)
	}
	assert.NotContains(t, got.raw, "OTHERKEY")
}

func TestQueue_should_return_422_for_a_cursor_with_a_NUL_or_bad_text(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopeMatchmakingRead)
	enc := func(raw string) string { return base64.RawURLEncoding.EncodeToString([]byte(raw)) }
	cursors := map[string]string{
		"NUL":          enc(`{"Mode":"a\u0000b","Region":"","GameMode":"","Status":"queued"}`),
		"not base64":   "!!!",
		"not the keys": enc(`[]`),
	}
	for name, c := range cursors {
		t.Run(name, func(t *testing.T) {
			// Act
			got := f.do(tok, http.MethodGet, queuePath+"?cursor="+c, "")

			// Assert
			assert.Equal(t, 422, got.status, got.raw)
		})
	}
}

func TestQueue_should_page_buckets_that_differ_only_in_status(t *testing.T) {
	// Arrange
	f := newFixture(t)
	for _, st := range []string{"queued", "matched", "cancelled"} {
		f.queuedWithStatus(f.projectA, "match_only", "eu", "duel", st)
	}
	tok := f.scopedToken(managementtoken.ScopeMatchmakingRead)

	// Act
	var seen []any
	cursor := ""
	for range 5 {
		got := f.do(tok, http.MethodGet, queuePath+"?limit=1&cursor="+cursor, "")
		for _, b := range got.body["buckets"].([]any) {
			seen = append(seen, b.(map[string]any)["status"])
		}
		next, _ := got.body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}

	// Assert
	assert.ElementsMatch(t, []any{"queued", "matched", "cancelled"}, seen)
}
