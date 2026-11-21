//go:build integration

package managementapi_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/managementtoken"
)

const (
	playerEmail       = "ann@example.com"
	playerDisplayName = "Ann the Great"
)

// namedPlayer adds a player with an email and a linked account that has a
// display name.
func (f *fixture) namedPlayer(projectID int64) int64 {
	f.t.Helper()
	return f.scalar(`WITH a AS (
			INSERT INTO player_accounts (email, password_hash, display_name) VALUES ($2, '\x00'::bytea, $3) RETURNING id
		)
		INSERT INTO project_players (tenant_id, project_id, external_id, email, player_account_id)
		SELECT p.tenant_id, p.id, 'ext-ann', $2, a.id FROM projects p, a WHERE p.id = $1 RETURNING id`,
		projectID, playerEmail, playerDisplayName)
}

func playerPath(id int64) string { return "/v1/project/players/" + strconv.FormatInt(id, 10) }

func TestPlayers_get_player_without_pii_should_have_no_email_and_no_display_name(t *testing.T) {
	// Arrange
	f := newFixture(t)
	id := f.namedPlayer(f.projectA)

	// Act
	got := f.do(f.scopedToken(managementtoken.ScopePlayersRead), http.MethodGet, playerPath(id), "")

	// Assert
	_, hasEmail := got.body["email"]
	_, hasName := got.body["display_name"]
	assert.Equal(t, []any{http.StatusOK, false, false, false, "ext-ann"},
		[]any{got.status, hasEmail, hasName, strings.Contains(got.raw, "example.com") || strings.Contains(got.raw, "Great"), got.body["external_id"]}, got.raw)
}

func TestPlayers_get_player_with_pii_should_have_email_and_display_name(t *testing.T) {
	// Arrange
	f := newFixture(t)
	id := f.namedPlayer(f.projectA)

	// Act
	got := f.do(f.scopedToken(managementtoken.ScopePlayersRead, managementtoken.ScopePlayersPII), http.MethodGet, playerPath(id), "")

	// Assert
	assert.Equal(t, []any{http.StatusOK, playerEmail, playerDisplayName}, []any{got.status, got.body["email"], got.body["display_name"]}, got.raw)
}

func TestPlayers_list_players_without_pii_should_have_no_email_and_no_display_name(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.namedPlayer(f.projectA)

	// Act
	got := f.do(f.scopedToken(managementtoken.ScopePlayersRead), http.MethodGet, "/v1/project/players", "")

	// Assert
	assert.Equal(t, []any{http.StatusOK, false}, []any{got.status, strings.Contains(got.raw, `"email"`) || strings.Contains(got.raw, "display_name")}, got.raw)
}

func TestPlayers_list_players_with_pii_should_have_email_and_display_name(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.namedPlayer(f.projectA)

	// Act
	got := f.do(f.scopedToken(managementtoken.ScopePlayersRead, managementtoken.ScopePlayersPII), http.MethodGet, "/v1/project/players", "")

	// Assert
	row := got.body["players"].([]any)[0].(map[string]any)
	assert.Equal(t, []any{playerEmail, playerDisplayName}, []any{row["email"], row["display_name"]}, got.raw)
}

func TestPlayers_should_return_403_without_players_read(t *testing.T) {
	f := newFixture(t)
	id := f.namedPlayer(f.projectA)
	tok := f.scopedToken(managementtoken.ScopePlayersPII, managementtoken.ScopeKeysRead)

	got := []int{f.get(tok, "/v1/project/players"), f.get(tok, playerPath(id))}

	assert.Equal(t, []int{http.StatusForbidden, http.StatusForbidden}, got)
}

func TestPlayers_should_return_403_when_the_creator_lacks_the_players_pair(t *testing.T) {
	// Arrange
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{managementtoken.ScopePlayersRead}, 3600e9)
	f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'project:*:players' AND v3 = 'manage'`)
	require.NoError(t, f.authz.ReloadPolicy())

	// Act
	got := f.do(tok, http.MethodGet, "/v1/project/players", "")

	// Assert
	assert.Equal(t, []any{http.StatusForbidden, true},
		[]any{got.status, strings.Contains(got.detail(), "does not have the manage permission")}, got.raw)
}

func TestPlayers_should_work_for_a_tenant_admin_creator(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{managementtoken.ScopePlayersRead}, 3600e9)

	got := f.get(tok, "/v1/project/players")

	assert.Equal(t, http.StatusOK, got)
}

func TestPlayers_should_not_find_players_of_other_projects(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok := f.scopedToken(managementtoken.ScopePlayersRead, managementtoken.ScopePlayersPII)
	otherProject := f.player(f.projectB)
	otherTenant := f.player(f.projectC)
	deleted := f.player(f.projectA)
	f.exec(`UPDATE project_players SET deleted_at = now() WHERE id = $1`, deleted)

	// Act
	got := map[string]int{
		"other project": f.get(tok, playerPath(otherProject)),
		"other tenant":  f.get(tok, playerPath(otherTenant)),
		"deleted":       f.get(tok, playerPath(deleted)),
	}

	// Assert
	assert.Equal(t, map[string]int{"other project": http.StatusNotFound, "other tenant": http.StatusNotFound, "deleted": http.StatusNotFound}, got)
}

func TestPlayers_list_should_return_only_players_of_the_token_project(t *testing.T) {
	// Arrange
	f := newFixture(t)
	mine := f.player(f.projectA)
	f.player(f.projectB)
	f.player(f.projectC)

	// Act
	got := f.do(f.scopedToken(managementtoken.ScopePlayersRead), http.MethodGet, "/v1/project/players", "")

	// Assert
	rows := got.body["players"].([]any)
	require.Len(t, rows, 1, got.raw)
	assert.EqualValues(t, mine, rows[0].(map[string]any)["id"])
}

func TestPlayers_list_should_filter_by_player_id_and_external_id(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.player(f.projectA)
	ann := f.namedPlayer(f.projectA)
	tok := f.scopedToken(managementtoken.ScopePlayersRead)
	other := f.player(f.projectB)

	// Act
	byID := f.do(tok, http.MethodGet, "/v1/project/players?player_id="+strconv.FormatInt(ann, 10), "")
	byExternal := f.do(tok, http.MethodGet, "/v1/project/players?external_id=ext-ann", "")
	noMatch := f.do(tok, http.MethodGet, "/v1/project/players?player_id="+strconv.FormatInt(other, 10), "")

	// Assert
	assert.Equal(t, []int{1, 1, 0},
		[]int{len(byID.body["players"].([]any)), len(byExternal.body["players"].([]any)), len(noMatch.body["players"].([]any))}, byID.raw)
}

func TestPlayers_list_should_page_all_rows_without_repeats(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO project_players (tenant_id, project_id, external_id)
		SELECT $1, $2, 'p-' || g FROM generate_series(1, 30) g`, f.tenantA, f.projectA)

	// Act
	ids := pageNames(t, f, f.scopedToken(managementtoken.ScopePlayersRead), "/v1/project/players", "players", "id", 12)

	// Assert
	assert.Len(t, slices.Compact(slices.Clone(ids)), 30)
}

func TestPlayers_tools_should_follow_the_scopes(t *testing.T) {
	f := newFixture(t)

	_, out := f.post(f.scopedToken(managementtoken.ScopePlayersRead), toolsList)

	assert.ElementsMatch(t, []string{"project_health_check", "get_token", "list_players", "get_player"}, toolNames(out))
}
