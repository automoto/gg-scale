//go:build integration

package managementapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/httpapi"
)

// writeCall is one valid request for a write operation and the scope it
// needs.
type writeCall struct {
	scope, method, path, body string
}

// writeCalls returns one valid request for each write operation. Each call
// is built fresh, so a test can run every call once.
func (f *fixture) writeCalls() map[string]writeCall {
	board := f.board(f.projectA, "b"+strconv.FormatInt(time.Now().UnixNano(), 36))
	deleted := f.board(f.projectA, "d"+strconv.FormatInt(time.Now().UnixNano(), 36))
	f.exec(`UPDATE leaderboards SET deleted_at = now() WHERE id = $1`, deleted)
	key := f.key(f.tenantA, f.projectA, "publishable", "k"+strconv.FormatInt(time.Now().UnixNano(), 36))
	return map[string]writeCall{
		"set_remote_config":       {"config:write", http.MethodPut, "/v1/project/remote-config", `{"config":{"a":1},"expected_revision":0}`},
		"rollback_remote_config":  {"config:write", http.MethodPost, "/v1/project/remote-config/rollback", `{"revision":1,"expected_revision":0}`},
		"create_leaderboard":      {"leaderboards:write", http.MethodPost, "/v1/project/leaderboards", fmt.Sprintf(`{"name":"new-%d"}`, board)},
		"update_leaderboard":      {"leaderboards:write", http.MethodPut, boardPath(board, ""), `{"expected_revision":0,"name":"renamed"}`},
		"rollback_leaderboard":    {"leaderboards:write", http.MethodPost, boardPath(board, "/rollback"), `{"revision":1,"expected_revision":0}`},
		"delete_leaderboard":      {"leaderboards:write", http.MethodDelete, boardPath(board, ""), ""},
		"restore_leaderboard":     {"leaderboards:write", http.MethodPost, boardPath(deleted, "/restore"), ""},
		"create_api_key":          {"keys:create", http.MethodPost, "/v1/project/api-keys", `{"label":"web build"}`},
		"update_api_key_label":    {"keys:write", http.MethodPut, keyPath(key, "/label"), `{"label":"renamed"}`},
		"update_api_key_features": {"keys:write", http.MethodPut, keyPath(key, "/features"), `{"scopes":["matchmaker"]}`},
		"set_allowed_origins":     {"origins:write", http.MethodPut, "/v1/project/allowed-origins", `{"origins":["http://localhost:5173"]}`},
	}
}

func TestProjectAPI_should_not_be_mounted_when_the_switch_is_off(t *testing.T) {
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementAPIEnabled = false })

	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	assert.Equal(t, http.StatusNotFound, got.status)
}

func TestProjectAPI_should_return_401_with_bearer_challenge_without_a_token(t *testing.T) {
	f := newFixture(t)
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/project/health", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, []any{http.StatusUnauthorized, "Bearer"}, []any{resp.StatusCode, resp.Header.Get("WWW-Authenticate")})
}

func TestProjectAPI_should_refuse_a_request_from_another_origin(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "", "Origin", "https://evil.example")

	assert.Equal(t, http.StatusForbidden, got.status)
}

func TestProjectAPI_should_permit_a_request_from_its_own_origin(t *testing.T) {
	f := newFixture(t)
	u, err := url.Parse(f.srv.URL)
	require.NoError(t, err)

	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "", "Origin", "http://"+u.Host)

	assert.Equal(t, http.StatusOK, got.status, got.raw)
}

func TestProjectAPI_reads_should_answer_200_with_a_json_object(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken(readScopes...)
	board := f.board(f.projectA, "b")
	ticket := f.ticket(f.projectA, f.player(f.projectA), "queued", `{}`)
	paths := []string{
		"/v1/project/health",
		"/v1/project/matchmaking/ticket-trace?ticket_id=" + strconv.FormatInt(ticket, 10),
		"/v1/project/remote-config",
		"/v1/project/leaderboards",
		boardPath(board, ""),
		"/v1/project/api-keys",
		"/v1/project/revisions?resource=remote_config",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			got := f.do(tok, http.MethodGet, path, "")

			assert.Equal(t, []any{http.StatusOK, true}, []any{got.status, got.body != nil}, got.raw)
		})
	}
}

func TestProjectAPI_writes_should_be_refused_without_their_scope(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken(readScopes...)

	for name, c := range f.writeCalls() {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, c.method, c.path, c.body)

			assert.Equal(t, []any{http.StatusForbidden, true}, []any{got.status, strings.Contains(got.detail(), c.scope)}, got.raw)
		})
	}
}

// The rollback operations need a kept revision first, so this test checks
// only that each operation gets past both permission checks.
func TestProjectAPI_writes_should_be_permitted_with_only_their_scope(t *testing.T) {
	f := newFixture(t)

	for name, c := range f.writeCalls() {
		t.Run(name, func(t *testing.T) {
			got := f.do(f.scopedToken(c.scope), c.method, c.path, c.body)

			assert.NotEqual(t, http.StatusForbidden, got.status, got.raw)
		})
	}
}

func TestProjectAPI_writes_should_answer_200_with_a_json_object(t *testing.T) {
	f := newFixture(t)
	calls := f.writeCalls()

	for _, name := range []string{"set_remote_config", "create_leaderboard", "update_leaderboard",
		"delete_leaderboard", "restore_leaderboard", "create_api_key", "update_api_key_label", "update_api_key_features", "set_allowed_origins"} {
		t.Run(name, func(t *testing.T) {
			c := calls[name]

			got := f.do(f.scopedToken(c.scope), c.method, c.path, c.body)

			assert.Equal(t, []any{http.StatusOK, true}, []any{got.status, got.body != nil}, got.raw)
		})
	}
}

func TestProjectAPI_should_be_refused_when_the_creator_lacks_the_pair(t *testing.T) {
	cases := map[string]struct {
		object, action, scope, method, path, body string
	}{
		"config update":       {"project:*:config", "update", "config:write", http.MethodPut, "/v1/project/remote-config", `{"config":{"a":1},"expected_revision":0}`},
		"leaderboard manage":  {"project:*:leaderboard", "manage", "leaderboards:write", http.MethodPost, "/v1/project/leaderboards", `{"name":"x"}`},
		"publishable key":     {"api_key:publishable", "manage", "keys:create", http.MethodPost, "/v1/project/api-keys", `{"label":"x"}`},
		"matchmaker read":     {"project:*:matchmaker", "read", "matchmaking:read", http.MethodGet, "/v1/project/matchmaking/ticket-trace?ticket_id=1", ""},
		"origins with config": {"project:*:config", "update", "origins:write", http.MethodPut, "/v1/project/allowed-origins", `{"origins":[]}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			admin := f.user("admin@example.com", f.tenantA, "admin")
			tok, _ := f.token(f.tenantA, f.projectA, admin, []string{c.scope}, time.Hour)
			f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = $1 AND v3 = $2`, c.object, c.action)
			require.NoError(t, f.authz.ReloadPolicy())

			got := f.do(tok, c.method, c.path, c.body)

			assert.Equal(t, []any{http.StatusForbidden, true},
				[]any{got.status, strings.Contains(got.detail(), "does not have the "+c.action+" permission")}, got.raw)
		})
	}
}

func TestProjectAPI_should_not_find_resources_of_other_projects(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("leaderboards:read", "leaderboards:write", "matchmaking:read")
	otherProject := f.board(f.projectB, "b")
	otherTenant := f.board(f.projectC, "c")
	deletedOther := f.board(f.projectB, "gone")
	f.exec(`UPDATE leaderboards SET deleted_at = now() WHERE id = $1`, deletedOther)
	ticketB := f.ticket(f.projectB, f.player(f.projectB), "queued", `{}`)
	ticketC := f.ticket(f.projectC, f.player(f.projectC), "queued", `{}`)
	cases := map[string]struct{ method, path, body string }{
		"get other project":      {http.MethodGet, boardPath(otherProject, ""), ""},
		"get other tenant":       {http.MethodGet, boardPath(otherTenant, ""), ""},
		"trace other project":    {http.MethodGet, "/v1/project/matchmaking/ticket-trace?ticket_id=" + strconv.FormatInt(ticketB, 10), ""},
		"trace other tenant":     {http.MethodGet, "/v1/project/matchmaking/ticket-trace?ticket_id=" + strconv.FormatInt(ticketC, 10), ""},
		"update other project":   {http.MethodPut, boardPath(otherProject, ""), `{"expected_revision":0,"name":"x"}`},
		"update other tenant":    {http.MethodPut, boardPath(otherTenant, ""), `{"expected_revision":0,"name":"x"}`},
		"rollback other project": {http.MethodPost, boardPath(otherProject, "/rollback"), `{"revision":1,"expected_revision":0}`},
		"delete other project":   {http.MethodDelete, boardPath(otherProject, ""), ""},
		"delete other tenant":    {http.MethodDelete, boardPath(otherTenant, ""), ""},
		"restore other project":  {http.MethodPost, boardPath(deletedOther, "/restore"), ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, c.method, c.path, c.body)

			assert.Equal(t, []any{http.StatusNotFound, "not found in this Game Project"}, []any{got.status, got.detail()}, got.raw)
		})
	}
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM leaderboards WHERE id IN ($1, $2) AND (deleted_at IS NOT NULL OR name = 'x')`, otherProject, otherTenant))
}

// The revisions of a board of another project are not shown: the list is
// empty, not a 404.
func TestProjectAPI_list_revisions_should_be_empty_for_a_board_of_another_project(t *testing.T) {
	f := newFixture(t)
	tokB, _ := f.token(f.tenantA, f.projectB, f.ownerID, []string{"leaderboards:write"}, time.Hour)
	created := f.do(tokB, http.MethodPost, "/v1/project/leaderboards", `{"name":"other"}`)
	require.Equal(t, http.StatusOK, created.status, created.raw)
	otherBoard := strconv.FormatInt(int64(created.body["leaderboard_id"].(float64)), 10)

	got := f.do(f.scopedToken(readScopes...), http.MethodGet, "/v1/project/revisions?resource=leaderboard&leaderboard_id="+otherBoard, "")

	assert.Equal(t, []any{http.StatusOK, []any{}}, []any{got.status, got.body["revisions"]}, got.raw)
}

func TestProjectAPI_rollback_remote_config_should_restore_the_kept_value(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("config:read", "config:write")
	first := f.do(tok, http.MethodPut, "/v1/project/remote-config", `{"config":{"a":1},"expected_revision":0}`)
	require.Equal(t, http.StatusOK, first.status, first.raw)
	second := f.do(tok, http.MethodPut, "/v1/project/remote-config",
		fmt.Sprintf(`{"config":{"a":2},"expected_revision":%v}`, first.body["revision"]))
	require.Equal(t, http.StatusOK, second.status, second.raw)

	got := f.do(tok, http.MethodPost, "/v1/project/remote-config/rollback",
		fmt.Sprintf(`{"revision":%v,"expected_revision":%v}`, first.body["revision"], second.body["revision"]))

	config := f.do(tok, http.MethodGet, "/v1/project/remote-config", "")
	assert.Equal(t, []any{http.StatusOK, map[string]any{"a": float64(1)}}, []any{got.status, config.body["config"]}, got.raw)
}

func TestProjectAPI_rollback_leaderboard_should_restore_the_kept_settings(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("leaderboards:read", "leaderboards:write")
	created := f.do(tok, http.MethodPost, "/v1/project/leaderboards", `{"name":"first"}`)
	require.Equal(t, http.StatusOK, created.status, created.raw)
	board := int64(created.body["leaderboard_id"].(float64))
	updated := f.do(tok, http.MethodPut, boardPath(board, ""),
		fmt.Sprintf(`{"expected_revision":%v,"name":"second"}`, created.body["revision"]))
	require.Equal(t, http.StatusOK, updated.status, updated.raw)

	got := f.do(tok, http.MethodPost, boardPath(board, "/rollback"),
		fmt.Sprintf(`{"revision":%v,"expected_revision":%v}`, created.body["revision"], updated.body["revision"]))

	read := f.do(tok, http.MethodGet, boardPath(board, ""), "")
	assert.Equal(t, []any{http.StatusOK, "first"}, []any{got.status, read.body["name"]}, got.raw)
}

func TestProjectAPI_set_remote_config_should_answer_400_for_a_nul_character(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("config:write"), http.MethodPut, "/v1/project/remote-config",
		`{"config":{"a":"\u0000"},"expected_revision":0}`)

	assert.Equal(t, http.StatusBadRequest, got.status, got.raw)
}

func TestProjectAPI_should_answer_413_for_a_body_over_the_limit(t *testing.T) {
	f := newFixture(t)
	body := `{"origins":["https://` + strings.Repeat("a", 131072) + `.example"]}`

	got := f.do(f.scopedToken("origins:write"), http.MethodPut, "/v1/project/allowed-origins", body)

	assert.Equal(t, http.StatusRequestEntityTooLarge, got.status)
}

func TestProjectAPI_attempt_cap_should_answer_422_above_the_int32_range(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("leaderboards:write")
	board := f.board(f.projectA, "b")
	cases := map[string]struct{ method, path, body string }{
		"create": {http.MethodPost, "/v1/project/leaderboards", `{"name":"c","attempt_cap":3000000000}`},
		"update": {http.MethodPut, boardPath(board, ""), `{"expected_revision":0,"name":"u","attempt_cap":3000000000}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, c.method, c.path, c.body)

			assert.Equal(t, []any{http.StatusUnprocessableEntity, false},
				[]any{got.status, strings.Contains(got.raw, "int32")}, got.raw)
		})
	}
}

func TestProjectAPI_cached_token_should_work_after_revoke_until_the_cache_time_ends(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementTokenCacheTTL, d.ManagementTokenCacheMaxEntries = time.Hour, 100 })
	tok, id := f.token(f.tenantA, f.projectA, f.ownerID, readScopes, time.Hour)
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodGet, "/v1/project/health", "").status)
	f.exec(`UPDATE management_tokens SET revoked_at = now() WHERE id = $1`, id)

	// Act
	got := f.do(tok, http.MethodGet, "/v1/project/health", "")

	// Assert
	assert.Equal(t, http.StatusOK, got.status, got.raw)
}

func TestProjectAPI_cached_token_should_stop_when_the_creator_loses_manage(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementTokenCacheTTL, d.ManagementTokenCacheMaxEntries = time.Hour, 100 })
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, readScopes, time.Hour)
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodGet, "/v1/project/health", "").status)
	require.NoError(t, f.authz.RemoveControlPanelRoles(admin, f.tenantA))
	require.NoError(t, f.authz.SetControlPanelMembershipRole(admin, f.tenantA, "member"))

	// Act
	got := f.do(tok, http.MethodGet, "/v1/project/health", "")

	// Assert
	assert.Equal(t, http.StatusForbidden, got.status, got.raw)
}

func TestProjectAPI_should_not_cache_a_token_when_the_creator_check_fails(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementTokenCacheTTL, d.ManagementTokenCacheMaxEntries = time.Hour, 100 })
	creator := f.user("member@example.com", f.tenantA, "member")
	tok, id := f.token(f.tenantA, f.projectA, creator, nil, time.Hour)
	require.Equal(t, http.StatusForbidden, f.do(tok, http.MethodGet, "/v1/project/health", "").status)
	f.exec(`UPDATE management_tokens SET revoked_at = now() WHERE id = $1`, id)
	require.NoError(t, f.authz.RemoveControlPanelRoles(creator, f.tenantA))
	require.NoError(t, f.authz.SetControlPanelMembershipRole(creator, f.tenantA, "admin"))

	// Act
	got := f.do(tok, http.MethodGet, "/v1/project/health", "")

	// Assert
	assert.Equal(t, http.StatusUnauthorized, got.status, got.raw)
}

func TestProjectAPI_should_not_cache_a_token_when_the_rate_limit_refuses(t *testing.T) {
	// Arrange
	f := newFixtureWith(t, func(d *httpapi.Deps) {
		d.ManagementTokenCacheTTL, d.ManagementTokenCacheMaxEntries = time.Hour, 100
		d.ManagementTokenRatePerSecond, d.ManagementTokenBurst = 0.001, 0
	})
	tok, id := f.token(f.tenantA, f.projectA, f.ownerID, nil, time.Hour)
	require.Equal(t, http.StatusTooManyRequests, f.do(tok, http.MethodGet, "/v1/project/health", "").status)
	f.exec(`UPDATE management_tokens SET revoked_at = now() WHERE id = $1`, id)

	// Act
	got := f.do(tok, http.MethodGet, "/v1/project/health", "")

	// Assert
	assert.Equal(t, http.StatusUnauthorized, got.status, got.raw)
}

func TestProjectAPI_cached_token_should_stop_when_it_expires(t *testing.T) {
	// Arrange
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	f := newFixtureWith(t, func(d *httpapi.Deps) {
		d.ManagementTokenCacheTTL, d.ManagementTokenCacheMaxEntries = 24*time.Hour, 100
		d.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	})
	tok, _ := f.token(f.tenantA, f.projectA, f.ownerID, readScopes, time.Hour)
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodGet, "/v1/project/health", "").status)
	clock.Add(int64(2 * time.Hour))

	// Act
	got := f.do(tok, http.MethodGet, "/v1/project/health", "")

	// Assert
	assert.Equal(t, []any{http.StatusUnauthorized, true}, []any{got.status, strings.Contains(got.raw, "token expired")}, got.raw)
}

func TestProjectAPI_should_refuse_requests_that_do_not_authenticate(t *testing.T) {
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.ManagementTokenRatePerSecond, d.ManagementTokenBurst = 0.001, 1 })
	revoked, revokedID := f.token(f.tenantA, f.projectA, f.ownerID, readScopes, time.Hour)
	f.exec(`UPDATE management_tokens SET revoked_at = now() WHERE id = $1`, revokedID)
	expired, expiredID := f.token(f.tenantA, f.projectA, f.ownerID, readScopes, time.Hour)
	f.exec(`UPDATE management_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiredID)
	ownerB := f.user("owner-b@example.com", f.tenantB, "owner")
	disabledTenant, _ := f.token(f.tenantB, f.projectC, ownerB, nil, time.Hour)
	f.exec(`UPDATE tenants SET disabled_at = now(), disabled_by = 'platform' WHERE id = $1`, f.tenantB)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	lostManage, _ := f.token(f.tenantA, f.projectA, admin, readScopes, time.Hour)
	require.NoError(t, f.authz.RemoveControlPanelRoles(admin, f.tenantA))
	require.NoError(t, f.authz.SetControlPanelMembershipRole(admin, f.tenantA, "member"))
	key := f.do(f.scopedToken("keys:create"), http.MethodPost, "/v1/project/api-keys", `{"label":"game"}`)
	require.Equal(t, http.StatusOK, key.status, key.raw)
	limited := f.scopedToken(readScopes...)
	require.Equal(t, http.StatusOK, f.do(limited, http.MethodGet, "/v1/project/health", "").status)
	cases := []struct {
		name   string
		token  string
		header []string
		status int
		text   string
	}{
		{"revoked token", revoked, nil, http.StatusUnauthorized, "unauthorized"},
		{"expired token", expired, nil, http.StatusUnauthorized, "token expired"},
		{"disabled tenant", disabledTenant, nil, http.StatusForbidden, "forbidden"},
		{"creator lost manage", lostManage, nil, http.StatusForbidden, "can no longer manage"},
		{"api key", key.body["api_key"].(string), nil, http.StatusUnauthorized, "unauthorized"},
		{"player session without a token", "", []string{"X-Session-Token", "session"}, http.StatusUnauthorized, "unauthorized"},
		{"over the api bucket rate", limited, nil, http.StatusTooManyRequests, "rate limit exceeded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := f.do(c.token, http.MethodGet, "/v1/project/health", "", c.header...)

			assert.Equal(t, []any{c.status, true}, []any{got.status, strings.Contains(got.raw, c.text)}, got.raw)
		})
	}
}

func TestProjectAPI_should_refuse_a_stale_expected_revision(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("config:write", "leaderboards:write")
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodPut, "/v1/project/remote-config", `{"config":{"a":1},"expected_revision":0}`).status)
	created := f.do(tok, http.MethodPost, "/v1/project/leaderboards", `{"name":"scores"}`)
	require.Equal(t, http.StatusOK, created.status, created.raw)
	board := int64(created.body["leaderboard_id"].(float64))
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodPut, boardPath(board, ""), `{"expected_revision":1,"name":"renamed"}`).status)

	cases := map[string]struct{ method, path, body string }{
		"set_remote_config":      {http.MethodPut, "/v1/project/remote-config", `{"config":{"a":2},"expected_revision":1}`},
		"rollback_remote_config": {http.MethodPost, "/v1/project/remote-config/rollback", `{"revision":1,"expected_revision":1}`},
		"update_leaderboard":     {http.MethodPut, boardPath(board, ""), `{"expected_revision":1,"name":"again"}`},
		"rollback_leaderboard":   {http.MethodPost, boardPath(board, "/rollback"), `{"revision":1,"expected_revision":1}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, c.method, c.path, c.body)

			assert.Equal(t, []any{http.StatusConflict, true},
				[]any{got.status, strings.Contains(got.detail(), "not the newest revision")}, got.raw)
		})
	}
}

func TestProjectAPI_set_remote_config_should_keep_large_integers_exact(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("config:write"), http.MethodPut, "/v1/project/remote-config",
		`{"config":{"id":12345678901234567890},"expected_revision":0}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)

	var config string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT remote_config::text FROM projects WHERE id = $1`, f.projectA).Scan(&config))
	assert.Contains(t, config, "12345678901234567890")
}

func TestProjectAPI_leaderboard_metadata_should_keep_large_integers_exact(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("leaderboards:write"), http.MethodPost, "/v1/project/leaderboards",
		`{"name":"big","metadata":{"id":12345678901234567890}}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)

	var metadata string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT metadata::text FROM leaderboards WHERE name = 'big'`).Scan(&metadata))
	assert.Contains(t, metadata, "12345678901234567890")
}

func TestProjectAPI_should_refuse_json_values_that_are_not_objects(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("config:write", "leaderboards:write")
	board := f.board(f.projectA, "b")
	cases := map[string]struct{ method, path, body string }{
		"config array":           {http.MethodPut, "/v1/project/remote-config", `{"config":[1],"expected_revision":0}`},
		"config string":          {http.MethodPut, "/v1/project/remote-config", `{"config":"{\"a\":1}","expected_revision":0}`},
		"config null":            {http.MethodPut, "/v1/project/remote-config", `{"config":null,"expected_revision":0}`},
		"config missing":         {http.MethodPut, "/v1/project/remote-config", `{"expected_revision":0}`},
		"create metadata number": {http.MethodPost, "/v1/project/leaderboards", `{"name":"x","metadata":5}`},
		"update metadata array":  {http.MethodPut, boardPath(board, ""), `{"expected_revision":0,"name":"x","metadata":[]}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, c.method, c.path, c.body)

			assert.Equal(t, http.StatusUnprocessableEntity, got.status, got.raw)
		})
	}
}

func TestProjectAPI_absent_and_null_metadata_should_mean_no_metadata(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("leaderboards:write")
	cases := map[string]string{
		"absent": `{"name":"absent"}`,
		"null":   `{"name":"null","metadata":null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, http.MethodPost, "/v1/project/leaderboards", body)
			require.Equal(t, http.StatusOK, got.status, got.raw)

			assert.Equal(t, 1, f.count(`SELECT count(*) FROM leaderboards WHERE name = $1 AND metadata IS NULL`, name))
		})
	}
}

func TestProjectAPI_get_leaderboard_should_omit_empty_metadata_and_keep_null_values(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken(readScopes...), http.MethodGet, boardPath(f.board(f.projectA, "b"), ""), "")

	_, hasMetadata := got.body["metadata"]
	scoreMin, hasScoreMin := got.body["score_min"]
	assert.Equal(t, []any{false, true, nil}, []any{hasMetadata, hasScoreMin, scoreMin}, got.raw)
}

func TestProjectAPI_list_leaderboards_should_show_deleted_only_when_asked(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken(readScopes...)

	plain := f.do(tok, http.MethodGet, "/v1/project/leaderboards", "")
	asked := f.do(tok, http.MethodGet, "/v1/project/leaderboards?include_deleted=true", "")

	_, inPlain := plain.body["deleted_leaderboards"]
	assert.Equal(t, []any{false, []any{}}, []any{inPlain, asked.body["deleted_leaderboards"]}, asked.raw)
}

func TestProjectAPI_list_leaderboards_should_use_snake_case_keys_for_deleted_boards(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("leaderboards:read", "leaderboards:write")
	f.do(tok, http.MethodDelete, boardPath(f.board(f.projectA, "gone"), ""), "")

	got := f.do(tok, http.MethodGet, "/v1/project/leaderboards?include_deleted=true", "")

	deleted, _ := got.body["deleted_leaderboards"].([]any)
	require.Len(t, deleted, 1, got.raw)
	keys := []string{}
	for k := range deleted[0].(map[string]any) {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"id", "name", "deleted_at"}, keys)
}

func TestProjectAPI_create_api_key_should_refuse_unknown_properties(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("keys:create"), http.MethodPost, "/v1/project/api-keys", `{"label":"x","key_type":"secret"}`)

	assert.Equal(t, []any{http.StatusUnprocessableEntity, 0}, []any{got.status, f.count(`SELECT count(*) FROM api_keys`)}, got.raw)
}

func TestProjectAPI_create_api_key_should_make_a_publishable_key_for_the_token_project(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("keys:create"), http.MethodPost, "/v1/project/api-keys", `{"label":"web build"}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)

	var projectID int64
	var keyType, label string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT project_id, key_type, label FROM api_keys`).Scan(&projectID, &keyType, &label))
	assert.Equal(t, []any{f.projectA, "publishable", "web build (management token: test)", true},
		[]any{projectID, keyType, label, strings.HasPrefix(got.body["api_key"].(string), "ggp_")})
}

func TestProjectAPI_create_api_key_should_refuse_a_scope_above_the_feature_grants(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("keys:create"), http.MethodPost, "/v1/project/api-keys", `{"label":"x","scopes":["p2p_relay"]}`)

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true, false},
		[]any{got.status, strings.Contains(got.detail(), "scope cannot be granted"), strings.Contains(got.raw, "projectadmin")}, got.raw)
}

func TestProjectAPI_create_api_key_should_answer_409_at_the_key_limit(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("keys:create")
	for i := range 3 {
		require.Equal(t, http.StatusOK, f.do(tok, http.MethodPost, "/v1/project/api-keys", `{"label":"k`+strconv.Itoa(i)+`"}`).status)
	}

	got := f.do(tok, http.MethodPost, "/v1/project/api-keys", `{"label":"one too many"}`)

	assert.Equal(t, []any{http.StatusConflict, true},
		[]any{got.status, strings.Contains(got.detail(), "maximum number of active API keys")}, got.raw)
}

func TestProjectAPI_set_allowed_origins_should_return_the_old_list(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("origins:write")
	f.do(tok, http.MethodPut, "/v1/project/allowed-origins", `{"origins":["https://a.example"]}`)

	got := f.do(tok, http.MethodPut, "/v1/project/allowed-origins", `{"origins":["http://localhost:5173"]}`)

	assert.Equal(t, []any{"https://a.example"}, got.body["old_origins"], got.raw)
}

func TestProjectAPI_set_allowed_origins_should_answer_422_for_an_invalid_origin(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("origins:write"), http.MethodPut, "/v1/project/allowed-origins", `{"origins":["https://*.example"]}`)

	assert.Equal(t, []any{http.StatusUnprocessableEntity, true},
		[]any{got.status, strings.Contains(got.detail(), "wildcard")}, got.raw)
}

func TestProjectAPI_set_allowed_origins_should_answer_409_above_the_limit(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("origins:write"), http.MethodPut, "/v1/project/allowed-origins",
		`{"origins":["https://a.example","https://b.example","https://c.example"]}`)

	assert.Equal(t, []any{http.StatusConflict, true}, []any{got.status, strings.Contains(got.detail(), "at most 2")}, got.raw)
}

func TestProjectAPI_write_should_record_the_token_as_actor(t *testing.T) {
	f := newFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"config:write"}, time.Hour)

	got := f.do(tok, http.MethodPut, "/v1/project/remote-config", `{"config":{"a":1},"expected_revision":0}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)

	var service string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT actor_service FROM platform_audit_log WHERE action = 'control_panel.remote_config.update'`).Scan(&service))
	assert.Equal(t, "management_token:"+strconv.FormatInt(tokenID, 10), service)
}

func TestProjectAPI_list_revisions_should_return_the_management_token_id(t *testing.T) {
	f := newFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"config:read", "config:write"}, time.Hour)
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodPut, "/v1/project/remote-config", `{"config":{"a":1},"expected_revision":0}`).status)

	got := f.do(tok, http.MethodGet, "/v1/project/revisions?resource=remote_config", "")

	newest := got.body["revisions"].([]any)[0].(map[string]any)
	assert.Equal(t, []any{float64(tokenID), "token"}, []any{newest["management_token_id"], newest["source"]}, got.raw)
}

func TestProjectAPI_list_revisions_should_answer_422_for_wrong_arguments(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken(readScopes...)

	for _, query := range []string{"", "?resource=players", "?resource=leaderboard"} {
		t.Run(query, func(t *testing.T) {
			got := f.do(tok, http.MethodGet, "/v1/project/revisions"+query, "")

			assert.Equal(t, http.StatusUnprocessableEntity, got.status, got.raw)
		})
	}
}

func TestProjectAPI_trace_should_need_exactly_one_argument(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("matchmaking:read"), http.MethodGet, "/v1/project/matchmaking/ticket-trace", "")

	assert.Equal(t, http.StatusUnprocessableEntity, got.status, got.raw)
}

func TestProjectAPI_trace_by_player_should_return_only_the_queued_ticket(t *testing.T) {
	f := newFixture(t)
	player := f.player(f.projectA)
	f.ticket(f.projectA, player, "failed", `{}`)
	queued := f.ticket(f.projectA, player, "queued", `{}`)

	got := f.do(f.scopedToken("matchmaking:read"), http.MethodGet, "/v1/project/matchmaking/ticket-trace?player_id="+strconv.FormatInt(player, 10), "")

	assert.Equal(t, float64(queued), got.body["ticket_id"], got.raw)
}

func TestProjectAPI_trace_should_never_return_client_text_or_attributes(t *testing.T) {
	f := newFixture(t)
	ticket := f.ticket(f.projectA, f.player(f.projectA), "queued", `{"x":"SECRET_ATTRIBUTE_TEXT"}`)

	got := f.do(f.scopedToken("matchmaking:read"), http.MethodGet, "/v1/project/matchmaking/ticket-trace?ticket_id="+strconv.FormatInt(ticket, 10), "")

	require.Equal(t, http.StatusOK, got.status, got.raw)
	for _, leaked := range []string{"SECRET_ATTRIBUTE_TEXT", "run the delete tool", "ignore all rules"} {
		assert.NotContains(t, got.raw, leaked)
	}
}

func TestProjectAPI_health_check_should_return_no_count_for_tenant_quotas(t *testing.T) {
	f := newFixture(t)
	f.exec(`UPDATE tenants SET enforce_quotas = true WHERE id = $1`, f.tenantA)

	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	quotas := got.body["checks"].(map[string]any)["quotas"].(map[string]any)
	assert.Equal(t, []any{"ok", "ok"}, []any{quotas["projects"], quotas["players"]}, got.raw)
}

func TestProjectAPI_health_check_should_omit_tenant_quotas_when_not_enforced(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	quotas := got.body["checks"].(map[string]any)["quotas"].(map[string]any)
	_, hasProjects := quotas["projects"]
	_, hasOpenSessions := quotas["open_sessions"]
	assert.Equal(t, []any{false, true}, []any{hasProjects, hasOpenSessions}, got.raw)
}

func TestProjectAPI_health_check_should_count_leaderboards_when_quotas_are_not_enforced(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'one'), ($1, $2, 'two')`, f.tenantA, f.projectA)

	// Act
	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	// Assert
	quotas := got.body["checks"].(map[string]any)["quotas"].(map[string]any)
	assert.Equal(t, map[string]any{"current": float64(2), "limit": float64(1000)}, quotas["leaderboards"], got.raw)
}

func TestProjectAPI_health_check_should_report_a_problem_at_the_leaderboard_limit(t *testing.T) {
	// Arrange
	f := newFixture(t)
	f.exec(`INSERT INTO tenant_quota_overrides (tenant_id, axis, "limit") VALUES ($1, 'leaderboards', 1)`, f.tenantA)
	f.exec(`INSERT INTO leaderboards (tenant_id, project_id, name) VALUES ($1, $2, 'one')`, f.tenantA, f.projectA)

	// Act
	got := f.do(f.scopedToken(), http.MethodGet, "/v1/project/health", "")

	// Assert
	assert.Contains(t, got.body["problems"], "The Game Project is at its leaderboard limit (1). New leaderboards are refused.", got.raw)
}

func TestProjectAPI_token_should_not_reach_another_project(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("config:read")
	f.exec(`UPDATE projects SET remote_config = '{"b":1}' WHERE id = $1`, f.projectB)

	got := f.do(tok, http.MethodGet, "/v1/project/remote-config", "")

	assert.Equal(t, map[string]any{}, got.body["config"], got.raw)
}
