//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/realtime"
	"github.com/automoto/gg-scale/internal/relay"
	"github.com/automoto/gg-scale/internal/serverlist"
	"github.com/automoto/gg-scale/internal/tenant"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestAPIKeyType_spec_matches_router sends a publishable key and a secret key
// to every operation of the real router. The publishable key must get 403
// exactly where the spec lists only the SecretKey scheme, and the secret key
// must never get 403. Both directions together fail if the key-type
// middleware or the casbin policy changes without the spec.
func TestAPIKeyType_spec_matches_router(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "key-type")
	// The scopes let the secret key pass the scope checks (fleet heartbeat),
	// so a 403 can only come from the key type or the casbin policy.
	var publishableID int64
	err := c.bootstrapPool.QueryRow(t.Context(),
		`UPDATE api_keys SET key_type='publishable', scopes=ARRAY['matchmaker','fleet','p2p_relay'] WHERE tenant_id=$1 AND project_id=$2 RETURNING id`,
		tenantID, projectID).Scan(&publishableID)
	require.NoError(t, err)
	// Give the publishable key the server role as a per-key grant, so casbin
	// lets it through and only the key-type middleware can refuse it. A
	// missing RequireKeyType(secret) then fails this test.
	_, err = c.bootstrapPool.Exec(t.Context(),
		`INSERT INTO casbin_rule (ptype, v0, v1, v2) VALUES ('g', $1, $2, $3)`,
		rbac.APIKeySubject(publishableID), rbac.RoleAPIServer, rbac.TenantDomain(tenantID))
	require.NoError(t, err)
	secretHash := sha256.Sum256([]byte("key-type-secret"))
	_, err = c.bootstrapPool.Exec(t.Context(),
		`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type, scopes) VALUES ($1, $2, $3, 'secret', ARRAY['matchmaker','fleet','p2p_relay'])`,
		tenantID, projectID, secretHash[:])
	require.NoError(t, err)
	// The fleet heartbeat also needs the dedicated servers feature; grant it
	// so its 403 can only come from the key checks.
	_, err = c.bootstrapPool.Exec(t.Context(),
		`INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, reason) VALUES ($1, $2, $3, true, 'integration test fixture')`,
		tenantID, projectID, string(rbac.FeatureDedicatedServers))
	require.NoError(t, err)
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Pool: pool, Lookup: tenant.NewSQLLookup(c.appPool), Limiter: ratelimit.NewCacheLimiter(c.cache),
		Signer: signer, Cache: c.cache, RBAC: authorizer, Matchmaker: matchmaker.NewPGQueue(pool),
		ServerList: serverlist.New(time.Minute), Hub: realtime.NewHub(),
		RelayIssuer: relay.NewIssuer("shared-relay-secret", "relay.test", time.Minute),
	}))
	t.Cleanup(srv.Close)

	doc := httpapi.OpenAPIDoc("1.0.0")
	for path, item := range doc.Paths {
		for method, op := range map[string]*huma.Operation{
			http.MethodGet: item.Get, http.MethodPut: item.Put, http.MethodPost: item.Post,
			http.MethodDelete: item.Delete, http.MethodPatch: item.Patch,
		} {
			if op == nil || len(op.Security) == 0 || path == "/v1/ws" || path == "/v1/healthz" {
				continue
			}
			url := srv.URL + pathParam.ReplaceAllString(path, "1")
			secret := secretKeyOnly(op)

			got := statusFor(t, method, url, "key-type")
			assert.Equal(t, secret, got == http.StatusForbidden,
				"%s %s: secret key only %v, publishable key got %d", method, path, secret, got)

			got = statusFor(t, method, url, "key-type-secret")
			assert.NotEqual(t, http.StatusForbidden, got, "%s %s: secret key got 403", method, path)
		}
	}
}

func secretKeyOnly(op *huma.Operation) bool {
	for _, req := range op.Security {
		if _, ok := req["SecretKey"]; !ok {
			return false
		}
	}
	return true
}

func statusFor(t *testing.T, method, url, key string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}
