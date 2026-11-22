//go:build integration

package managementapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
)

const secretKeysPath = "/v1/project/secret-api-keys"

// createdKeyID makes a publishable key through create_api_key, so the key
// has its Casbin role as a real key has, and returns its ID.
func (f *fixture) createdKeyID() int64 {
	f.t.Helper()
	got := f.do(f.scopedToken("keys:create"), http.MethodPost, "/v1/project/api-keys", `{"label":"game"}`)
	require.Equal(f.t, http.StatusOK, got.status, got.raw)
	return int64(got.body["api_key_id"].(float64))
}

func (f *fixture) keyRevoked(id int64) bool {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NOT NULL`, id) == 1
}

func TestRevokeAPIKey_should_revoke_remove_the_role_and_audit_the_token(t *testing.T) {
	f := newFixture(t)
	id := f.createdKeyID()
	require.Equal(t, 1, f.count(`SELECT count(*) FROM casbin_rule WHERE ptype = 'g' AND v0 = $1`, rbac.APIKeySubject(id)))
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"keys:revoke"}, time.Hour)

	got := f.do(tok, http.MethodPost, keyPath(id, "/revoke"), "")
	require.Equal(t, http.StatusOK, got.status, got.raw)

	roles := f.count(`SELECT count(*) FROM casbin_rule WHERE ptype = 'g' AND v0 = $1`, rbac.APIKeySubject(id))
	audits := f.count(`SELECT count(*) FROM platform_audit_log WHERE action = 'control_panel.api_key.revoke'
		AND actor_service = $1 AND target = $2 AND (payload->>'project_id')::bigint = $3`,
		"management_token:"+strconv.FormatInt(tokenID, 10), strconv.FormatInt(id, 10), f.projectA)
	assert.Equal(t, []any{true, true, 0, 1}, []any{got.body["revoked"], f.keyRevoked(id), roles, audits})
}

func TestRevokeAPIKey_should_reset_the_rate_limit_bucket_of_the_key(t *testing.T) {
	store := memory.New()
	f := newFixtureWith(t, func(d *httpapi.Deps) { d.Cache = store })
	id := f.key(f.tenantA, f.projectA, "publishable", "k")
	bucket := ratelimit.APIKeyBucketKey(id)
	_, _, err := store.TokenBucket(context.Background(), bucket, 1, 0.01, 1)
	require.NoError(t, err)

	got := f.do(f.scopedToken("keys:revoke"), http.MethodPost, keyPath(id, "/revoke"), "")
	require.Equal(t, http.StatusOK, got.status, got.raw)

	allowed, _, err := store.TokenBucket(context.Background(), bucket, 1, 0.01, 1)
	require.NoError(t, err)
	assert.True(t, allowed)
}

func TestRevokeAPIKey_should_return_404_for_a_key_out_of_reach(t *testing.T) {
	f := newFixture(t)
	otherProject := f.key(f.tenantA, f.projectB, "publishable", "b")
	otherTenant := f.key(f.tenantB, f.projectC, "publishable", "c")
	tenantWide := f.scalar(`INSERT INTO api_keys (tenant_id, key_hash, key_type, label, scopes)
		VALUES ($1, sha256('wide'::bytea), 'publishable', 'wide', '{matchmaker}') RETURNING id`, f.tenantA)
	revoked := f.key(f.tenantA, f.projectA, "publishable", "gone")
	f.exec(`UPDATE api_keys SET revoked_at = now() - interval '1 day' WHERE id = $1`, revoked)
	tok := f.scopedToken("keys:revoke")
	var statuses []int
	for _, id := range []int64{otherProject, otherTenant, tenantWide, revoked, 999999} {
		statuses = append(statuses, f.do(tok, http.MethodPost, keyPath(id, "/revoke"), "").status)
	}

	assert.Equal(t, []any{[]int{404, 404, 404, 404, 404}, 0, 0},
		[]any{statuses, f.count(`SELECT count(*) FROM api_keys WHERE revoked_at > now() - interval '1 hour'`),
			f.count(`SELECT count(*) FROM platform_audit_log WHERE action = 'control_panel.api_key.revoke'`)})
}

func TestRevokeAPIKey_should_return_403_without_the_keys_revoke_scope(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "k")
	tok := f.scopedToken("keys:read", "keys:create", "keys:write", "keys:secret")

	got := f.do(tok, http.MethodPost, keyPath(id, "/revoke"), "")

	assert.Equal(t, []any{http.StatusForbidden, true, false},
		[]any{got.status, strings.Contains(got.detail(), "keys:revoke"), f.keyRevoked(id)}, got.raw)
}

func TestRevokeAPIKey_should_return_403_when_the_creator_lacks_the_publishable_pair(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "k")
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{"keys:revoke"}, time.Hour)
	f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'api_key:publishable' AND v3 = 'manage'`)
	require.NoError(t, f.authz.ReloadPolicy())

	got := f.do(tok, http.MethodPost, keyPath(id, "/revoke"), "")

	assert.Equal(t, []any{http.StatusForbidden, false}, []any{got.status, f.keyRevoked(id)}, got.raw)
}

func TestRevokeAPIKey_should_return_403_for_a_secret_key_when_the_creator_lacks_the_secret_pair(t *testing.T) {
	f := newFixture(t)
	secret := f.key(f.tenantA, f.projectA, "secret", "srv")
	public := f.key(f.tenantA, f.projectA, "publishable", "pub")
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{"keys:revoke"}, time.Hour)
	f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'api_key:secret' AND v3 = 'manage'`)
	require.NoError(t, f.authz.ReloadPolicy())

	denied := f.do(tok, http.MethodPost, keyPath(secret, "/revoke"), "")
	allowed := f.do(tok, http.MethodPost, keyPath(public, "/revoke"), "")

	assert.Equal(t, []any{http.StatusForbidden, http.StatusOK, false, true},
		[]any{denied.status, allowed.status, f.keyRevoked(secret), f.keyRevoked(public)}, denied.raw+allowed.raw)
}

func TestRevokeAPIKey_tool_should_revoke_the_key(t *testing.T) {
	f := newFixture(t)
	id := f.key(f.tenantA, f.projectA, "publishable", "k")

	res := f.call(f.scopedToken("keys:revoke"), "revoke_api_key", map[string]any{"api_key_id": id})

	assert.Equal(t, []any{false, true}, []any{isError(res), f.keyRevoked(id)}, resultText(res))
}

// lockedBuffer is a log sink that parallel tests may write at once.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs sends the default logger to a buffer at debug level until the
// test ends.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestCreateSecretAPIKey_should_make_a_secret_key_for_the_token_project(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("keys:secret"), http.MethodPost, secretKeysPath, `{"label":"ci deploy"}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)

	value, _ := got.body["api_key"].(string)
	var projectID int64
	var keyType, label string
	var hash []byte
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT project_id, key_type, label, key_hash FROM api_keys`).Scan(&projectID, &keyType, &label, &hash))
	sum := sha256.Sum256([]byte(value))
	assert.Equal(t, []any{true, "secret", f.projectA, "secret", "ci deploy (management token: test)", sum[:]},
		[]any{strings.HasPrefix(value, "ggs_"), got.body["key_type"], projectID, keyType, label, hash})
}

func TestCreateSecretAPIKey_value_should_be_in_the_create_response_only(t *testing.T) {
	f := newFixture(t)
	logs := captureLogs(t)
	slog.Info("secret key log capture is on")

	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{"keys:secret"}, time.Hour)
	created := f.do(tok, http.MethodPost, secretKeysPath, `{"label":"ci"}`)
	require.Equal(t, http.StatusOK, created.status, created.raw)
	fromTool, _ := structured(f.call(tok, "create_secret_api_key", map[string]any{"label": "ci tool"}))["api_key"].(string)
	values := []string{created.body["api_key"].(string), fromTool}
	require.True(t, strings.HasPrefix(values[0], "ggs_") && strings.HasPrefix(values[1], "ggs_"), created.raw)
	reader := f.scopedToken(readScopes...)
	list := f.do(reader, http.MethodGet, "/v1/project/api-keys", "")
	token := f.do(reader, http.MethodGet, "/v1/project/token", "")
	tool := resultText(f.call(reader, "list_api_keys", map[string]any{}))
	var audit string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT string_agg(payload::text, ' ') FROM platform_audit_log WHERE action = 'control_panel.api_key.create'
		AND actor_service = $1 AND payload->>'key_type' = 'secret' HAVING count(*) = 2`,
		"management_token:"+strconv.FormatInt(tokenID, 10)).Scan(&audit))
	leaked := []string{}
	for _, value := range values {
		for name, text := range map[string]string{"log": logs.String(), "list": list.raw, "list tool": tool, "token": token.raw, "audit": audit} {
			if strings.Contains(text, value) {
				leaked = append(leaked, name)
			}
		}
	}

	assert.Equal(t, []any{true, true, []string{}},
		[]any{strings.Contains(logs.String(), "secret key log capture is on"), strings.Contains(list.raw, `"label":"ci tool`), leaked})
}

func TestCreateSecretAPIKey_should_return_403_when_the_creator_lacks_the_secret_pair(t *testing.T) {
	f := newFixture(t)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{"keys:secret"}, time.Hour)
	f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'api_key:secret' AND v3 = 'manage'`)
	require.NoError(t, f.authz.ReloadPolicy())

	got := f.do(tok, http.MethodPost, secretKeysPath, `{"label":"ci"}`)

	assert.Equal(t, []any{http.StatusForbidden, 0}, []any{got.status, f.count(`SELECT count(*) FROM api_keys`)}, got.raw)
}

func TestCreateSecretAPIKey_should_return_403_with_only_keys_create(t *testing.T) {
	f := newFixture(t)

	got := f.do(f.scopedToken("keys:create"), http.MethodPost, secretKeysPath, `{"label":"ci"}`)

	assert.Equal(t, []any{http.StatusForbidden, 0}, []any{got.status, f.count(`SELECT count(*) FROM api_keys`)}, got.raw)
}

func TestCreateSecretAPIKey_should_share_the_key_limit_with_create_api_key(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("keys:create", "keys:secret")
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodPost, "/v1/project/api-keys", `{"label":"p1"}`).status)
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodPost, "/v1/project/api-keys", `{"label":"p2"}`).status)
	require.Equal(t, http.StatusOK, f.do(tok, http.MethodPost, secretKeysPath, `{"label":"s1"}`).status)

	secret := f.do(tok, http.MethodPost, secretKeysPath, `{"label":"s2"}`)
	public := f.do(tok, http.MethodPost, "/v1/project/api-keys", `{"label":"p3"}`)

	assert.Equal(t, []int{http.StatusConflict, http.StatusConflict}, []int{secret.status, public.status}, secret.raw+public.raw)
}

func TestCreateSecretAPIKey_should_return_422_for_a_bad_label_or_scope(t *testing.T) {
	f := newFixture(t)
	tok := f.scopedToken("keys:secret")

	empty := f.do(tok, http.MethodPost, secretKeysPath, `{"label":""}`)
	scope := f.do(tok, http.MethodPost, secretKeysPath, `{"label":"x","scopes":["p2p_relay"]}`)

	assert.Equal(t, []any{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, 0},
		[]any{empty.status, scope.status, f.count(`SELECT count(*) FROM api_keys`)}, empty.raw+scope.raw)
}

func TestCreateSecretAPIKey_tool_should_return_a_secret_key(t *testing.T) {
	f := newFixture(t)

	res := f.call(f.scopedToken("keys:secret"), "create_secret_api_key", map[string]any{"label": "ci"})

	value, _ := structured(res)["api_key"].(string)
	assert.Equal(t, []any{false, true}, []any{isError(res), strings.HasPrefix(value, "ggs_")}, resultText(res))
}
