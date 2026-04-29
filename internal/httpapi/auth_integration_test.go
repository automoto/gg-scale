//go:build integration

package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/httpapi"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/tenant"
)

type cluster struct {
	bootstrapPool *pgxpool.Pool
	appPool       *pgxpool.Pool
	valkey        *redis.Client
}

func startCluster(t *testing.T) *cluster {
	t.Helper()
	ctx := context.Background()

	pgCtr, err := tcpostgres.Run(ctx,
		"postgres:17",
		tcpostgres.WithDatabase("ggscale_test"),
		tcpostgres.WithUsername("ggscale"),
		tcpostgres.WithPassword("ggscale"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pgCtr.Terminate(shutdown)
	})

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "db", "migrations"))
	require.NoError(t, err)
	r, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, r.Up())
	require.NoError(t, r.Close())

	bootstrap, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(bootstrap.Close)

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	valCtr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "valkey/valkey:8",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = valCtr.Terminate(shutdown)
	})

	host, err := valCtr.Host(ctx)
	require.NoError(t, err)
	port, err := valCtr.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)

	valkey := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	require.NoError(t, valkey.Ping(ctx).Err())
	t.Cleanup(func() { _ = valkey.Close() })

	return &cluster{bootstrapPool: bootstrap, appPool: app, valkey: valkey}
}

func seedTenantWithAPIKey(t *testing.T, pool *pgxpool.Pool, tier, token string) (tenantID, projectID int64) {
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
	signer, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)

	h := httpapi.NewRouter(httpapi.Deps{
		Version: "v1",
		Commit:  "test",
		Pool:    db.NewPool(c.appPool),
		Lookup:  tenant.NewSQLLookup(c.appPool),
		Limiter: ratelimit.NewValkeyLimiter(c.valkey),
		Signer:  signer,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func TestAuthAnonymous_creates_end_user_signs_jwt_persists_session(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, "free", "test-token")
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
		EndUserID    int64  `json:"end_user_id"`
		ExternalID   string `json:"external_id"`
		ExpiresAt    string `json:"expires_at"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))

	assert.NotEmpty(t, body.AccessToken)
	assert.NotEmpty(t, body.RefreshToken)
	assert.Greater(t, body.EndUserID, int64(0))
	assert.Contains(t, body.ExternalID, "anon_")

	// Verify the JWT decodes to the right claims.
	signer, _ := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	claims, err := signer.Verify(body.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, tenantID, claims.TenantID)
	assert.Equal(t, projectID, claims.ProjectID)
	assert.Equal(t, body.EndUserID, claims.EndUserID)

	// Verify the session row was persisted with the hashed refresh token.
	sum := sha256.Sum256([]byte(body.RefreshToken))
	var sessionEndUserID int64
	require.NoError(t, c.bootstrapPool.QueryRow(context.Background(),
		`SELECT end_user_id FROM sessions WHERE refresh_hash = $1`, sum[:]).Scan(&sessionEndUserID))
	assert.Equal(t, body.EndUserID, sessionEndUserID)
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
