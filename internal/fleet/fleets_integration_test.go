//go:build integration

package fleet_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/migrate"
)

func startMigratedDB(t *testing.T) (*db.Pool, int64, int64) {
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

	raw, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	pool := db.NewPool(raw)

	var tenantID, projectID int64
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('fleet-store-test') RETURNING id`).Scan(&tenantID))
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p') RETURNING id`,
		tenantID).Scan(&projectID))
	return pool, tenantID, projectID
}

func TestPostgresFleetStore_Create_then_GetByID_and_GetByName(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	store := fleet.NewPostgresFleetStore(pool)
	ctx := db.WithTenant(context.Background(), tenantID)

	created, err := store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID,
		Name:      "primary",
		Backend:   "docker",
		Config: map[string]string{
			"image": "traefik/whoami:latest",
			"port":  "80",
		},
	})
	require.NoError(t, err)
	require.NotZero(t, created.ID)
	assert.Equal(t, tenantID, created.TenantID)
	assert.Equal(t, projectID, created.ProjectID)
	assert.Equal(t, "docker", created.Backend)
	assert.Equal(t, "traefik/whoami:latest", created.Config["image"])

	got, err := store.GetByID(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "primary", got.Name)

	byName, err := store.GetByName(ctx, projectID, "primary")
	require.NoError(t, err)
	assert.Equal(t, created.ID, byName.ID)
}

func TestPostgresFleetStore_GetByName_returns_ErrFleetNotFound(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	store := fleet.NewPostgresFleetStore(pool)
	ctx := db.WithTenant(context.Background(), tenantID)

	_, err := store.GetByName(ctx, projectID, "nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, fleet.ErrFleetNotFound))
}

func TestPostgresFleetStore_ListForProject_skips_deleted(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	store := fleet.NewPostgresFleetStore(pool)
	ctx := db.WithTenant(context.Background(), tenantID)

	a, err := store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID, Name: "a", Backend: "docker",
		Config: map[string]string{"image": "x:1", "port": "80"},
	})
	require.NoError(t, err)
	_, err = store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID, Name: "b", Backend: "docker",
		Config: map[string]string{"image": "y:1", "port": "81"},
	})
	require.NoError(t, err)

	require.NoError(t, store.SoftDelete(ctx, a.ID))

	out, err := store.ListForProject(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "b", out[0].Name)
}

func TestPostgresFleetStore_Update_changes_config_but_not_backend(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	store := fleet.NewPostgresFleetStore(pool)
	ctx := db.WithTenant(context.Background(), tenantID)

	f, err := store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID, Name: "primary", Backend: "docker",
		Config: map[string]string{"image": "x:1", "port": "80"},
	})
	require.NoError(t, err)

	err = store.Update(ctx, fleet.FleetUpdate{
		ID:      f.ID,
		Name:    "primary",
		Backend: "docker",
		Config:  map[string]string{"image": "x:2", "port": "8080"},
	})
	require.NoError(t, err)

	got, err := store.GetByID(ctx, f.ID)
	require.NoError(t, err)
	assert.Equal(t, "x:2", got.Config["image"])
	assert.Equal(t, "8080", got.Config["port"])
}

func TestPostgresFleetStore_SoftDelete_lets_same_name_recreate(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	store := fleet.NewPostgresFleetStore(pool)
	ctx := db.WithTenant(context.Background(), tenantID)

	first, err := store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID, Name: "primary", Backend: "docker",
		Config: map[string]string{"image": "x:1", "port": "80"},
	})
	require.NoError(t, err)
	require.NoError(t, store.SoftDelete(ctx, first.ID))

	// Same name should be re-usable because the unique index is partial
	// on deleted_at IS NULL.
	second, err := store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID, Name: "primary", Backend: "docker",
		Config: map[string]string{"image": "x:2", "port": "80"},
	})
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID)

	// GetByName resolves to the live row, not the deleted one.
	got, err := store.GetByName(ctx, projectID, "primary")
	require.NoError(t, err)
	assert.Equal(t, second.ID, got.ID)
}

func TestPostgresFleetStore_Create_rejects_duplicate_active_name(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	store := fleet.NewPostgresFleetStore(pool)
	ctx := db.WithTenant(context.Background(), tenantID)

	_, err := store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID, Name: "primary", Backend: "docker",
		Config: map[string]string{"image": "x:1", "port": "80"},
	})
	require.NoError(t, err)

	_, err = store.Create(ctx, fleet.FleetCreate{
		ProjectID: projectID, Name: "primary", Backend: "docker",
		Config: map[string]string{"image": "x:1", "port": "80"},
	})
	require.Error(t, err, "active duplicate-name insert must violate the unique index")
}

func TestPostgresFleetStore_isolated_per_tenant(t *testing.T) {
	pool, tenantA, projectA := startMigratedDB(t)

	store := fleet.NewPostgresFleetStore(pool)
	ctxA := db.WithTenant(context.Background(), tenantA)
	_, err := store.Create(ctxA, fleet.FleetCreate{
		ProjectID: projectA, Name: "primary", Backend: "docker",
		Config: map[string]string{"image": "x", "port": "80"},
	})
	require.NoError(t, err)

	// RLS check using a tenant-scoped context that points at a different
	// tenant id: GetByName must return ErrFleetNotFound, not return tenant
	// A's row.
	ctxOther := db.WithTenant(context.Background(), tenantA+9999)
	_, err = store.GetByName(ctxOther, projectA, "primary")
	require.Error(t, err)
	assert.True(t, errors.Is(err, fleet.ErrFleetNotFound))

	// ListForProject under the wrong tenant returns nothing too.
	out, err := store.ListForProject(ctxOther, projectA)
	require.NoError(t, err)
	assert.Empty(t, out)
}
