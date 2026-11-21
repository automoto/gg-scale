//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/realtime"
	"github.com/automoto/gg-scale/internal/relay"
	"github.com/automoto/gg-scale/internal/serverlist"
	"github.com/automoto/gg-scale/internal/tenant"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestAPIKeyType_spec_matches_router sends a publishable key, a secret key,
// and a management token to every operation of the real router. The
// publishable key must get 403 exactly where the spec lists only the
// SecretKey scheme, and the secret key must never get 403. Both directions
// together fail if the key-type middleware or the casbin policy changes
// without the spec. On an operation with the ManagementToken scheme, both
// API keys get 401 and the token gets past authentication; on each other
// operation, the token gets 401.
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
	// A management token of the tenant owner with every scope, so a 403 on
	// a /v1/project operation can only come from a wrong scheme.
	var ownerID int64
	err = c.bootstrapPool.QueryRow(t.Context(),
		`INSERT INTO control_panel_users (email, password_hash, email_verified_at) VALUES ('key-type-owner@example.com', '\x00'::bytea, now()) RETURNING id`).Scan(&ownerID)
	require.NoError(t, err)
	require.NoError(t, authorizer.SetControlPanelMembershipRole(ownerID, tenantID, "owner"))
	scopes := []string{}
	for _, s := range managementtoken.Scopes {
		scopes = append(scopes, s.Scope)
	}
	managementHash := sha256.Sum256([]byte(keyTypeManagementToken))
	_, err = c.bootstrapPool.Exec(t.Context(),
		`INSERT INTO management_tokens (tenant_id, project_id, created_by_user_id, label, token_hash, token_hint, scopes, expires_at)
		VALUES ($1, $2, $3, 'key-type', $4, 'hint', $5, now() + interval '1 hour')`,
		tenantID, projectID, ownerID, managementHash[:], scopes)
	require.NoError(t, err)
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Pool: pool, Lookup: tenant.NewSQLLookup(c.appPool), Limiter: ratelimit.NewCacheLimiter(c.cache),
		Signer: signer, Cache: c.cache, RBAC: authorizer, Matchmaker: matchmaker.NewPGQueue(pool),
		ServerList: serverlist.New(time.Minute), Hub: realtime.NewHub(),
		RelayIssuer: relay.NewIssuer("shared-relay-secret", "relay.test", time.Minute),
		// Each request comes from its own client IP (see statusFor), so the
		// per-IP buckets for unknown keys and failed token logins never
		// change an answer.
		ProxyTrust:           ratelimit.NewProxyTrust(clientIPHeader, []string{"127.0.0.0/8", "::1/128"}),
		ManagementAPIEnabled: true, ManagementTokenRatePerSecond: 1000, ManagementTokenBurst: 1000,
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
			if hasScheme(op, "ManagementToken") {
				got := []int{statusFor(t, method, url, "key-type"), statusFor(t, method, url, "key-type-secret")}
				assert.Equal(t, []int{http.StatusUnauthorized, http.StatusUnauthorized}, got,
					"%s %s: API keys must get 401", method, path)

				// The token gets past authentication: no 401 and no 403.
				status := statusFor(t, method, url, keyTypeManagementToken)
				assert.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status,
					"%s %s: management token got %d", method, path, status)
				continue
			}
			secret := secretKeyOnly(op)

			got := statusFor(t, method, url, "key-type")
			assert.Equal(t, secret, got == http.StatusForbidden,
				"%s %s: secret key only %v, publishable key got %d", method, path, secret, got)

			got = statusFor(t, method, url, "key-type-secret")
			assert.NotEqual(t, http.StatusForbidden, got, "%s %s: secret key got 403", method, path)

			if hasScheme(op, "PlatformAdminToken") {
				continue
			}
			got = statusFor(t, method, url, keyTypeManagementToken)
			assert.Equal(t, http.StatusUnauthorized, got, "%s %s: management token got %d", method, path, got)
		}
	}
}

// keyTypeManagementToken is the management token of the key-type test.
const keyTypeManagementToken = "ggm_key-type"

// clientIPHeader carries the client IP that statusFor sends; the router of
// the key-type test trusts it from loopback.
const clientIPHeader = "X-Test-Client-IP"

var clientIPs atomic.Uint32

func hasScheme(op *huma.Operation, scheme string) bool {
	for _, req := range op.Security {
		if _, ok := req[scheme]; ok {
			return true
		}
	}
	return false
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
	n := clientIPs.Add(1)
	req.Header.Set(clientIPHeader, fmt.Sprintf("10.%d.%d.%d", n/65536%256, n/256%256, n%256))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}
