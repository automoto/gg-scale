//go:build integration

package managementapi_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// key inserts an API key and returns its ID.
func (f *fixture) key(tenantID, projectID int64, keyType, label string, scopes ...string) int64 {
	f.t.Helper()
	if scopes == nil {
		scopes = []string{"matchmaker"}
	}
	return f.scalar(`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type, label, scopes)
		VALUES ($1, $2, sha256($3::bytea), $4, $5, $6) RETURNING id`,
		tenantID, projectID, []byte(label+strconv.FormatInt(time.Now().UnixNano(), 36)), keyType, label, scopes)
}

func keyPath(id int64, suffix string) string {
	return "/v1/project/api-keys/" + strconv.FormatInt(id, 10) + suffix
}

func TestAPIKeyWrite_should_return_403_without_the_keys_write_scope(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "old")
	tok := f.scopedToken("keys:read", "keys:create")

	label := f.do(tok, http.MethodPut, keyPath(id, "/label"), `{"label":"new"}`)
	features := f.do(tok, http.MethodPut, keyPath(id, "/features"), `{"scopes":[]}`)

	assert.Equal(t, []int{http.StatusForbidden, http.StatusForbidden}, []int{label.status, features.status})
}

func TestAPIKeyWrite_should_return_403_when_the_creator_lacks_the_publishable_pair(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "old")
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{"keys:write"}, time.Hour)
	f.dropPair("role:tenant_admin", "api_key:publishable", "manage")

	got := f.do(tok, http.MethodPut, keyPath(id, "/label"), `{"label":"new"}`)

	assert.Equal(t, http.StatusForbidden, got.status, got.raw)
}

func TestAPIKeyWrite_should_return_403_for_a_secret_key_when_the_creator_lacks_the_secret_pair(t *testing.T) {
	f := newFixture(t)
	secret := f.key(f.tenantA, f.projectA, "secret", "srv")
	public := f.key(f.tenantA, f.projectA, "publishable", "pub")
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{"keys:write"}, time.Hour)
	f.dropPair("role:tenant_admin", "api_key:secret", "manage")

	denied := f.do(tok, http.MethodPut, keyPath(secret, "/label"), `{"label":"new"}`)
	allowed := f.do(tok, http.MethodPut, keyPath(public, "/label"), `{"label":"new"}`)

	assert.Equal(t, []int{http.StatusForbidden, http.StatusOK}, []int{denied.status, allowed.status}, denied.raw+allowed.raw)
}

func TestAPIKeyWrite_should_return_404_for_a_key_of_another_project_or_tenant(t *testing.T) {
	f := newFixture(t)
	otherProject := f.key(f.tenantA, f.projectB, "publishable", "b")
	otherTenant := f.key(f.tenantB, f.projectC, "publishable", "c")
	tenantWide := f.scalar(`INSERT INTO api_keys (tenant_id, key_hash, key_type, label, scopes)
		VALUES ($1, sha256('wide'::bytea), 'publishable', 'wide', '{matchmaker}') RETURNING id`, f.tenantA)
	tok := f.scopedToken("keys:write")
	var statuses []int
	for _, id := range []int64{otherProject, otherTenant, tenantWide, 999999} {
		statuses = append(statuses,
			f.do(tok, http.MethodPut, keyPath(id, "/label"), `{"label":"x"}`).status,
			f.do(tok, http.MethodPut, keyPath(id, "/features"), `{"scopes":[]}`).status)
	}

	assert.Equal(t, []int{404, 404, 404, 404, 404, 404, 404, 404}, statuses)
	assert.Equal(t, 0, f.count(`SELECT count(*) FROM api_keys WHERE label = 'x'`))
}

func TestAPIKeyWrite_label_should_change_the_label_and_audit_the_token(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "old")
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"keys:write"}, time.Hour)

	got := f.do(tok, http.MethodPut, keyPath(id, "/label"), `{"label":"  new name "}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)

	var label, service, target string
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT label FROM api_keys WHERE id = $1`, id).Scan(&label))
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT actor_service, target FROM platform_audit_log WHERE action = 'control_panel.api_key.relabel'`).Scan(&service, &target))
	assert.Equal(t, []string{"new name", "management_token:" + strconv.FormatInt(tokenID, 10), strconv.FormatInt(id, 10)},
		[]string{label, service, target})
}

func TestAPIKeyWrite_label_should_return_422_for_a_bad_label(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "old")
	tok := f.scopedToken("keys:write")

	empty := f.do(tok, http.MethodPut, keyPath(id, "/label"), `{"label":""}`)
	long := f.do(tok, http.MethodPut, keyPath(id, "/label"), `{"label":"`+strings.Repeat("a", 65)+`"}`)

	assert.Equal(t, []int{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity}, []int{empty.status, long.status})
}

func TestAPIKeyWrite_features_should_revoke_a_scope_and_audit_the_token(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "k", "matchmaker", "fleet")
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"keys:write"}, time.Hour)

	got := f.do(tok, http.MethodPut, keyPath(id, "/features"), `{"scopes":["matchmaker"]}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)

	var scopes []string
	var service string
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT scopes FROM api_keys WHERE id = $1`, id).Scan(&scopes))
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT actor_service FROM platform_audit_log WHERE action = 'control_panel.api_key.scope_revoke'`).Scan(&service))
	assert.Equal(t, []any{[]string{"matchmaker"}, "management_token:" + strconv.FormatInt(tokenID, 10)}, []any{scopes, service})
}

func TestAPIKeyWrite_features_should_return_422_for_a_scope_the_project_lacks(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "k")
	tok := f.scopedToken("keys:write")

	got := f.do(tok, http.MethodPut, keyPath(id, "/features"), `{"scopes":["matchmaker","p2p_relay"]}`)
	unknown := f.do(tok, http.MethodPut, keyPath(id, "/features"), `{"scopes":["admin"]}`)

	assert.Equal(t, []int{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity}, []int{got.status, unknown.status}, got.raw+unknown.raw)
}

func TestAPIKeyWrite_should_return_404_for_a_revoked_key(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "old", "matchmaker", "fleet")
	f.exec(`UPDATE api_keys SET revoked_at = now() WHERE id = $1`, id)
	tok := f.scopedToken("keys:write")

	label := f.do(tok, http.MethodPut, keyPath(id, "/label"), `{"label":"new"}`)
	features := f.do(tok, http.MethodPut, keyPath(id, "/features"), `{"scopes":["matchmaker"]}`)

	assert.Equal(t, []int{http.StatusNotFound, http.StatusNotFound, 0},
		[]int{label.status, features.status, f.count(`SELECT count(*) FROM api_keys WHERE label = 'new' OR scopes = '{matchmaker}'`)})
}

func TestAPIKeyWrite_features_should_return_422_and_keep_scopes_when_scopes_is_null(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "k", "matchmaker", "fleet")
	tok, _ := f.token(f.tenantA, f.projectA, f.ownerID, []string{"keys:write"}, time.Hour)

	got := f.do(tok, http.MethodPut, keyPath(id, "/features"), `{"scopes":null}`)

	var scopes []string
	require.NoError(t, f.owner.QueryRow(context.Background(), `SELECT scopes FROM api_keys WHERE id = $1`, id).Scan(&scopes))
	assert.Equal(t, []any{http.StatusUnprocessableEntity, 2}, []any{got.status, len(scopes)})
}
