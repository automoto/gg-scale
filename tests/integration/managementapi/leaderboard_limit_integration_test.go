//go:build integration

package managementapi_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectAPI_create_leaderboard_at_limit_should_answer_409(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit") VALUES ($1, 'leaderboards', 2)`, f.tenantA)
	tok := f.scopedToken("leaderboards:write")
	for i := range 2 {
		got := f.do(tok, http.MethodPost, "/v1/project/leaderboards", fmt.Sprintf(`{"name":"b-%d"}`, i))
		require.Equal(t, http.StatusOK, got.status, got.raw)
	}

	// Act
	got := f.do(tok, http.MethodPost, "/v1/project/leaderboards", `{"name":"one-more"}`)

	// Assert
	assert.Equal(t, []any{http.StatusConflict, true},
		[]any{got.status, strings.Contains(got.detail(), "maximum number of leaderboards")}, got.raw)
}

// pageNames follows next_cursor of path until it is empty and returns the
// value of field in each row of list, in order.
func pageNames(t *testing.T, f *fixture, tok, path, list, field string, limit int) []string {
	t.Helper()
	var seen []string
	cursor := ""
	for range 20 {
		got := f.do(tok, http.MethodGet, fmt.Sprintf("%s?limit=%d&cursor=%s", path, limit, cursor), "")
		require.Equal(t, http.StatusOK, got.status, got.raw)
		rows := got.body[list].([]any)
		require.LessOrEqual(t, len(rows), limit, got.raw)
		for _, r := range rows {
			seen = append(seen, fmt.Sprint(r.(map[string]any)[field]))
		}
		next, _ := got.body["next_cursor"].(string)
		if next == "" {
			return seen
		}
		cursor = next
	}
	t.Fatal("paging did not end")
	return nil
}

func TestProjectAPI_list_leaderboards_should_page_all_rows_without_repeats(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO leaderboards (tenant_id, project_id, name)
		SELECT $1, $2, 'b-' || lpad(g::text, 3, '0') FROM generate_series(1, 60) g`, f.tenantA, f.projectA)

	// Act
	names := pageNames(t, f, f.scopedToken(readScopes...), "/v1/project/leaderboards", "leaderboards", "name", 25)

	// Assert
	assert.Len(t, slices.Compact(slices.Clone(names)), 60)
}

func TestProjectAPI_list_api_keys_should_page_all_rows_without_repeats(t *testing.T) {
	// Arrange: 55 keys, four of them revoked or in another project.
	f := newFixture(t)
	f.exec(`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type, label)
		SELECT $1, $2, sha256(('k' || g)::bytea), 'publishable', 'k-' || g FROM generate_series(1, 55) g`,
		f.tenantA, f.projectA)
	f.exec(`UPDATE api_keys SET revoked_at = now() WHERE label IN ('k-1', 'k-2')`)
	f.exec(`UPDATE api_keys SET project_id = $1 WHERE label IN ('k-3', 'k-4')`, f.projectB)

	// Act
	ids := pageNames(t, f, f.scopedToken(readScopes...), "/v1/project/api-keys", "api_keys", "id", 20)

	// Assert
	assert.Len(t, slices.Compact(slices.Clone(ids)), 51)
}

func TestProjectAPI_get_token_should_return_the_token_facts(t *testing.T) {
	// Arrange
	f := newFixture(t)
	tok, id := f.token(f.tenantA, f.projectA, f.ownerID, []string{"keys:read", "config:write"}, time.Hour)

	// Act
	got := f.do(tok, http.MethodGet, "/v1/project/token", "")

	// Assert
	assert.Equal(t, []any{http.StatusOK, float64(id), "test", float64(f.projectA), "pa", float64(f.tenantA),
		[]any{"keys:read", "config:write"}, []any{"keys:read", "config:write"}, true},
		[]any{got.status, got.body["token_id"], got.body["label"], got.body["project_id"], got.body["project_name"],
			got.body["tenant_id"], got.body["scopes"], got.body["active_scopes"], got.body["expires_at"] != nil}, got.raw)
}

func TestProjectAPI_get_token_should_open_to_a_token_with_no_scopes(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/token", "")

	assert.Equal(t, []any{http.StatusOK, []any{}}, []any{got.status, got.body["active_scopes"]}, got.raw)
}
