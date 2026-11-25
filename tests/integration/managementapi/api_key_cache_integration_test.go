//go:build integration

package managementapi_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/tenant"
)

const cachedGameKey = "cached-game-key"

// newKeyCacheFixture puts one API key cache in front of the key lookup, as
// the server does, and counts the database lookups. The TTL is long, so only
// a drop on write can make the cache read the key again. It returns the ID of
// a key of project A that is already in the cache.
func newKeyCacheFixture(t *testing.T) (f *fixture, keyID int64, lookups *atomic.Int64) {
	t.Helper()
	lookups = &atomic.Int64{}
	keys := tenant.NewKeyCache(func(ctx context.Context, hash []byte) (*tenant.APIKey, error) {
		lookups.Add(1)
		return tenant.NewSQLLookup(f.owner)(ctx, hash)
	}, time.Hour, 100)
	// The game API mounts only with a signer.
	signer, err := auth.NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	f = newFixtureWith(t, func(d *httpapi.Deps) {
		d.Lookup = keys.Lookup
		d.APIKeyCache = keys
		d.Signer = signer
	})
	keyID = f.scalar(`INSERT INTO api_keys (tenant_id, project_id, key_hash, key_type, label, scopes)
		VALUES ($1, $2, sha256($3::bytea), 'publishable', 'game', '{matchmaker,fleet}') RETURNING id`,
		f.tenantA, f.projectA, []byte(cachedGameKey))
	require.Equal(t, http.StatusOK, f.gameKeyStatus(), "the key must work, and be cached, before the write")
	return f, keyID, lookups
}

// gameKeyStatus sends one game API request with the cached key.
func (f *fixture) gameKeyStatus() int {
	f.t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.srv.URL+"/v1/config", nil)
	require.NoError(f.t, err)
	req.Header.Set("Authorization", "Bearer "+cachedGameKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(f.t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestAPIKeyCache_should_refuse_a_cached_key_at_once_after_a_token_revokes_it(t *testing.T) {
	f, keyID, _ := newKeyCacheFixture(t)
	tok := f.scopedToken("keys:revoke")

	got := f.do(tok, http.MethodPost, keyPath(keyID, "/revoke"), "")
	require.Equal(t, http.StatusOK, got.status, got.raw)

	assert.Equal(t, http.StatusForbidden, f.gameKeyStatus())
}

func TestAPIKeyCache_should_read_a_cached_key_again_after_a_token_changes_its_scopes(t *testing.T) {
	f, keyID, lookups := newKeyCacheFixture(t)
	tok := f.scopedToken("keys:write")

	got := f.do(tok, http.MethodPut, keyPath(keyID, "/features"), `{"scopes":["matchmaker"]}`)
	require.Equal(t, http.StatusOK, got.status, got.raw)
	f.gameKeyStatus()

	assert.Equal(t, int64(2), lookups.Load())
}
