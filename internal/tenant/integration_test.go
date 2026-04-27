//go:build integration

package tenant_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/tenant"
)

// startCluster brings up a postgres container, applies migrations, and
// returns two pools:
//   - bootstrap: superuser pool used to seed test data (RLS bypassed)
//   - app:       runs every connection as ggscale_app via AfterConnect, so
//                queries actually exercise RLS like production will
func startCluster(t *testing.T) (bootstrap, app *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:17",
		tcpostgres.WithDatabase("ggscale_test"),
		tcpostgres.WithUsername("ggscale"),
		tcpostgres.WithPassword("ggscale"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = ctr.Terminate(shutdownCtx)
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "db", "migrations"))
	require.NoError(t, err)

	r, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, r.Up())
	require.NoError(t, r.Close())

	bootstrap, err = pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(bootstrap.Close)

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	app, err = pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	return bootstrap, app
}

func seedTenant(t *testing.T, pool *pgxpool.Pool, name string) (tenantID, projectID int64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, name).Scan(&tenantID))
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, $2) RETURNING id`,
		tenantID, name+"-default").Scan(&projectID))
	return
}

func seedEndUser(t *testing.T, pool *pgxpool.Pool, tenantID, projectID int64, externalID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO end_users (tenant_id, project_id, external_id)
		 VALUES ($1, $2, $3) RETURNING id`,
		tenantID, projectID, externalID).Scan(&id))
	return id
}

func seedAPIKey(t *testing.T, pool *pgxpool.Pool, tenantID, projectID int64, token string) {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	_, err := pool.Exec(context.Background(),
		`INSERT INTO api_keys (tenant_id, project_id, key_hash) VALUES ($1, $2, $3)`,
		tenantID, projectID, sum[:])
	require.NoError(t, err)
}

func TestRLS_isolates_end_users_across_tenants_with_overlapping_external_ids(t *testing.T) {
	bootstrap, app := startCluster(t)

	tenantA, projectA := seedTenant(t, bootstrap, "tenant-a")
	tenantB, projectB := seedTenant(t, bootstrap, "tenant-b")
	idA := seedEndUser(t, bootstrap, tenantA, projectA, "shared-external-id")
	idB := seedEndUser(t, bootstrap, tenantB, projectB, "shared-external-id")
	require.NotEqual(t, idA, idB)

	p := db.NewPool(app)

	// Under tenant A's context only A's row is visible.
	var visibleIDs []int64
	require.NoError(t, p.Q(db.WithTenant(context.Background(), tenantA), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT id FROM end_users WHERE external_id = 'shared-external-id'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			visibleIDs = append(visibleIDs, id)
		}
		return rows.Err()
	}))
	assert.Equal(t, []int64{idA}, visibleIDs, "tenant A must only see A's row")

	// Under tenant B's context only B's row is visible.
	visibleIDs = visibleIDs[:0]
	require.NoError(t, p.Q(db.WithTenant(context.Background(), tenantB), func(tx pgx.Tx) error {
		rows, err := tx.Query(context.Background(),
			`SELECT id FROM end_users WHERE external_id = 'shared-external-id'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			visibleIDs = append(visibleIDs, id)
		}
		return rows.Err()
	}))
	assert.Equal(t, []int64{idB}, visibleIDs, "tenant B must only see B's row")
}

func TestRLS_blocks_cross_tenant_storage_reads(t *testing.T) {
	bootstrap, app := startCluster(t)

	tenantA, projectA := seedTenant(t, bootstrap, "a")
	tenantB, projectB := seedTenant(t, bootstrap, "b")
	userA := seedEndUser(t, bootstrap, tenantA, projectA, "alice")
	userB := seedEndUser(t, bootstrap, tenantB, projectB, "bob")

	for _, row := range []struct {
		tenantID, projectID, userID int64
		key, value                  string
	}{
		{tenantA, projectA, userA, "save", `{"hp": 100}`},
		{tenantB, projectB, userB, "save", `{"hp": 50}`},
	} {
		_, err := bootstrap.Exec(context.Background(),
			`INSERT INTO storage_objects (tenant_id, project_id, owner_user_id, key, value)
			 VALUES ($1, $2, $3, $4, $5::jsonb)`,
			row.tenantID, row.projectID, row.userID, row.key, row.value)
		require.NoError(t, err)
	}

	p := db.NewPool(app)

	for _, tc := range []struct {
		tenantID    int64
		expectCount int
	}{
		{tenantA, 1},
		{tenantB, 1},
	} {
		var count int
		require.NoError(t, p.Q(db.WithTenant(context.Background(), tc.tenantID), func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(),
				`SELECT COUNT(*) FROM storage_objects`).Scan(&count)
		}))
		assert.Equal(t, tc.expectCount, count, "tenant %d", tc.tenantID)
	}
}

func TestSQLLookup_resolves_token_to_tenant_via_bootstrap_policy(t *testing.T) {
	bootstrap, app := startCluster(t)

	tenantA, projectA := seedTenant(t, bootstrap, "a")
	seedAPIKey(t, bootstrap, tenantA, projectA, "secret-token")

	lookup := tenant.NewSQLLookup(app)
	sum := sha256.Sum256([]byte("secret-token"))
	key, err := lookup(context.Background(), sum[:])

	require.NoError(t, err)
	assert.Equal(t, tenantA, key.TenantID)
	require.NotNil(t, key.ProjectID)
	assert.Equal(t, projectA, *key.ProjectID)
	assert.False(t, key.Revoked)
}

func TestSQLLookup_returns_ErrUnknownKey_for_missing_token(t *testing.T) {
	_, app := startCluster(t)

	lookup := tenant.NewSQLLookup(app)
	sum := sha256.Sum256([]byte("nope"))
	_, err := lookup(context.Background(), sum[:])

	assert.ErrorIs(t, err, tenant.ErrUnknownKey)
}

func TestMiddleware_end_to_end_against_real_DB(t *testing.T) {
	bootstrap, app := startCluster(t)

	tenantA, projectA := seedTenant(t, bootstrap, "a")
	seedAPIKey(t, bootstrap, tenantA, projectA, "live-token")

	mw := tenant.New(tenant.NewSQLLookup(app))

	var ctxSeen context.Context
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxSeen = r.Context()
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer live-token")
	rr := httptest.NewRecorder()
	mw(handler).ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	tid, err := db.TenantFromContext(ctxSeen)
	require.NoError(t, err)
	assert.Equal(t, tenantA, tid)
}

func TestMiddleware_revoked_key_in_DB_returns_403(t *testing.T) {
	bootstrap, app := startCluster(t)

	tenantA, projectA := seedTenant(t, bootstrap, "a")
	sum := sha256.Sum256([]byte("revoked-token"))
	_, err := bootstrap.Exec(context.Background(),
		`INSERT INTO api_keys (tenant_id, project_id, key_hash, revoked_at)
		 VALUES ($1, $2, $3, now())`,
		tenantA, projectA, sum[:])
	require.NoError(t, err)

	mw := tenant.New(tenant.NewSQLLookup(app))

	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer revoked-token")
	rr := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("revoked key must not reach the handler")
	})).ServeHTTP(rr, req)

	assert.Equal(t, http.StatusForbidden, rr.Code)
}
