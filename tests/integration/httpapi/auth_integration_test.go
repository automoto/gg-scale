//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/cache/memory"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

// testSignerKey is the HMAC key every integration test signs and
// verifies tokens with. Kept here (alongside the cluster fixture) so
// the value is impossible to drift between the server-under-test
// (`newServerForCluster` + `newFullStackServer`) and the tokens tests
// hand-roll for negative cases.
const testSignerKey = "test-key-must-be-at-least-32-bytes-long"

// newTestSigner builds a Signer over testSignerKey. Fails the test on
// any constructor error; callers don't need to handle it.
func newTestSigner(t *testing.T) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	return s
}

type cluster struct {
	bootstrapPool *pgxpool.Pool
	appPool       *pgxpool.Pool
	cache         cache.Store
}

const httpapiTemplateDB = "ggscale_httpapi_template"

type httpapiPostgresFixture struct {
	ctr         *tcpostgres.PostgresContainer
	admin       *pgxpool.Pool
	templateDSN string
	seq         atomic.Uint64
	err         error
}

var (
	httpapiPGOnce sync.Once
	httpapiPG     *httpapiPostgresFixture
)

func TestMain(m *testing.M) {
	code := m.Run()
	if httpapiPG != nil {
		httpapiPG.close()
	}
	os.Exit(code)
}

func startCluster(t *testing.T) *cluster {
	t.Helper()
	t.Parallel()
	ctx := context.Background()
	pg := sharedHTTPAPIPostgres(t)
	dbName, dsn := pg.createDatabase(t)

	bootstrap, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)

	store := memory.New()
	t.Cleanup(func() {
		_ = store.Close(context.Background())
		app.Close()
		bootstrap.Close()
		pg.dropDatabase(dbName)
	})

	return &cluster{bootstrapPool: bootstrap, appPool: app, cache: store}
}

func sharedHTTPAPIPostgres(t *testing.T) *httpapiPostgresFixture {
	t.Helper()
	httpapiPGOnce.Do(func() {
		httpapiPG = &httpapiPostgresFixture{}
		httpapiPG.err = httpapiPG.start(context.Background())
	})
	require.NoError(t, httpapiPG.err)
	return httpapiPG
}

func (p *httpapiPostgresFixture) start(ctx context.Context) error {
	ctr, err := tcpostgres.Run(ctx,
		"postgres:17",
		tcpostgres.WithDatabase(httpapiTemplateDB),
		tcpostgres.WithUsername("ggscale"),
		tcpostgres.WithPassword("ggscale"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return err
	}
	p.ctr = ctr

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return err
	}
	p.templateDSN = dsn

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "..", "db", "migrations"))
	if err != nil {
		return err
	}
	r, err := migrate.New(dsn, migrationsDir)
	if err != nil {
		return err
	}
	if err := r.Up(); err != nil {
		_ = r.Close()
		return err
	}
	if err := r.Close(); err != nil {
		return err
	}

	adminDSN, err := postgresDSNForDatabase(dsn, "postgres")
	if err != nil {
		return err
	}
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return err
	}
	p.admin = admin
	return nil
}

func (p *httpapiPostgresFixture) createDatabase(t *testing.T) (string, string) {
	t.Helper()
	dbName := fmt.Sprintf("ggscale_httpapi_%d", p.seq.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := p.admin.Exec(ctx,
		"CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()+
			" WITH TEMPLATE "+pgx.Identifier{httpapiTemplateDB}.Sanitize()+
			" OWNER ggscale")
	require.NoError(t, err)
	dsn, err := postgresDSNForDatabase(p.templateDSN, dbName)
	require.NoError(t, err)
	return dbName, dsn
}

func (p *httpapiPostgresFixture) dropDatabase(dbName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = p.admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
	_, _ = p.admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
}

func (p *httpapiPostgresFixture) close() {
	if p.admin != nil {
		p.admin.Close()
	}
	if p.ctr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = p.ctr.Terminate(ctx)
	}
}

func postgresDSNForDatabase(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + dbName
	return u.String(), nil
}

func seedTenantWithAPIKey(t *testing.T, pool *pgxpool.Pool, tier int16, token string) (tenantID, projectID int64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name, tier) VALUES ($1, $2) RETURNING id`,
		"tenant-"+token, tier).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, $2) RETURNING id`,
		tenantID, "project-"+token).Scan(&projectID))
	sum := sha256.Sum256([]byte(token))
	_, err := pool.Exec(ctx,
		`INSERT INTO api_keys (tenant_id, project_id, key_hash) VALUES ($1, $2, $3)`,
		tenantID, projectID, sum[:])
	require.NoError(t, err)
	return
}

func newServerForCluster(t *testing.T, c *cluster) *httptest.Server {
	t.Helper()
	signer, err := auth.NewSigner([]byte(testSignerKey))
	require.NoError(t, err)
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authorizer.Close)

	h := httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
		Pool:    pool,
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Limiter: ratelimit.NewCacheLimiter(c.cache),
		Signer:  signer,
		Cache:   c.cache,
		RBAC:    authorizer,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestAuthAnonymous_creates_player_signs_jwt_persists_session(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "test-token")
	srv := newServerForCluster(t, c)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/auth/anonymous", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		PlayerID     int64  `json:"player_id"`
		ExternalID   string `json:"external_id"`
		ExpiresAt    string `json:"expires_at"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	assert.NotEmpty(t, body.AccessToken)
	assert.NotEmpty(t, body.RefreshToken)
	assert.Greater(t, body.PlayerID, int64(0))
	assert.Contains(t, body.ExternalID, "anon_")

	// Verify the JWT decodes to the right claims.
	signer, _ := auth.NewSigner([]byte(testSignerKey))
	claims, err := signer.Verify(body.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, tenantID, claims.TenantID)
	assert.Equal(t, projectID, claims.ProjectID)
	assert.Equal(t, body.PlayerID, claims.PlayerID)

	// Verify the session row was persisted with the hashed refresh token.
	sum := sha256.Sum256([]byte(body.RefreshToken))
	var sessionPlayerID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT player_id FROM sessions WHERE refresh_hash = $1`, sum[:]).Scan(&sessionPlayerID))
	assert.Equal(t, body.PlayerID, sessionPlayerID)
}

func TestAuthAnonymous_returns_401_without_api_key(t *testing.T) {
	c := startCluster(t)
	srv := newServerForCluster(t, c)

	resp, err := http.Post(srv.URL+"/v1/auth/anonymous", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAuthAnonymous_returns_401_with_unknown_api_key(t *testing.T) {
	c := startCluster(t)
	srv := newServerForCluster(t, c)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/auth/anonymous", nil)
	req.Header.Set("Authorization", "Bearer ghost-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHealthz_remains_public_with_auth_deps_present(t *testing.T) {
	c := startCluster(t)
	srv := newServerForCluster(t, c)

	resp, err := http.Get(srv.URL + "/v1/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
