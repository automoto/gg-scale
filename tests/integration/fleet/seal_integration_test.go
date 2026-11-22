//go:build integration

package fleet_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/secretseal"
)

const sealMarker = "tok-SEALMARKER-7f3a"

func testCipher(t *testing.T) *secretseal.Cipher {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	c, err := secretseal.NewCipherKeyring(key)
	require.NoError(t, err)
	return c
}

// storedConfig reads the fleets.config column as the database holds it.
func storedConfig(t *testing.T, ctx context.Context, pool *db.Pool, id int64) string {
	t.Helper()
	var raw string
	require.NoError(t, pool.Q(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT config::text FROM fleets WHERE id = $1`, id).Scan(&raw)
	}))
	return raw
}

func createPluginFleet(t *testing.T, ctx context.Context, pool *db.Pool, c *secretseal.Cipher, tenantID, projectID int64, token string) *fleet.Fleet {
	t.Helper()
	f, err := fleet.NewPostgresFleetStore(pool, c).CreateFleet(ctx, tenantID, fleet.FleetCreate{
		ProjectID: projectID, Name: "sealed", Backend: "plugin:x",
		Config: map[string]string{"api_token": token, "region": "GRA9"},
	}, actor)
	require.NoError(t, err)
	return f
}

func TestCreateFleet_should_not_store_a_plain_plugin_value(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)
	c := testCipher(t)

	f := createPluginFleet(t, ctx, pool, c, tenantID, projectID, sealMarker)

	raw := storedConfig(t, ctx, pool, f.ID)
	assert.NotContains(t, raw, sealMarker)
	assert.Contains(t, raw, `"api_token"`)
	assert.Equal(t, sealMarker, f.Config["api_token"])
}

func TestPostgresFleetStore_should_read_a_sealed_value_as_plain(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)
	c := testCipher(t)
	f := createPluginFleet(t, ctx, pool, c, tenantID, projectID, sealMarker)

	got, err := fleet.NewPostgresFleetStore(pool, c).GetByID(ctx, f.ID)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"api_token": sealMarker, "region": "GRA9"}, got.Config)
}

func TestUpdateFleet_should_not_store_a_plain_plugin_value(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)
	c := testCipher(t)
	f := createPluginFleet(t, ctx, pool, c, tenantID, projectID, "old-token")
	next := sealMarker + "-new"

	updated, err := fleet.NewPostgresFleetStore(pool, c).UpdateFleet(ctx, tenantID, projectID, f.ID, nil, map[string]string{"api_token": next}, actor)

	require.NoError(t, err)
	assert.NotContains(t, storedConfig(t, ctx, pool, f.ID), next)
	assert.Equal(t, next, updated.Config["api_token"])
}

func TestRestoreFleet_should_keep_the_value_sealed(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)
	c := testCipher(t)
	f := createPluginFleet(t, ctx, pool, c, tenantID, projectID, sealMarker)
	require.NoError(t, fleet.NewPostgresFleetStore(pool, nil).DeleteFleet(ctx, tenantID, projectID, f.ID, actor))

	restored, err := fleet.NewPostgresFleetStore(pool, c).RestoreFleet(ctx, tenantID, projectID, f.ID, actor)

	require.NoError(t, err)
	assert.NotContains(t, storedConfig(t, ctx, pool, f.ID), sealMarker)
	assert.Equal(t, sealMarker, restored.Config["api_token"])
}

// insertPlainPluginFleet writes a row as the previous release stored it.
func insertPlainPluginFleet(t *testing.T, ctx context.Context, pool *db.Pool, tenantID, projectID int64) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.Q(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO fleets (tenant_id, project_id, name, backend, config)
			 VALUES ($1, $2, 'legacy', 'plugin', jsonb_build_object('api_token', $3::text))
			 RETURNING id`, tenantID, projectID, sealMarker).Scan(&id)
	}))
	return id
}

type recordingBackend struct {
	fakeBackend
	got map[string]string
}

func (b *recordingBackend) Allocate(_ context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	b.got = req.Config
	return &fleet.Allocation{BackendRef: "ref-1", Address: "127.0.0.1:7777", Status: fleet.StatusReady}, nil
}

func TestManagerAllocate_should_give_the_plugin_the_plain_value(t *testing.T) {
	tests := []struct {
		name   string
		legacy bool
	}{
		{"sealed_row", false},
		{"old_plain_row", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, tenantID, projectID := startMigratedDB(t)
			ctx := db.WithTenant(context.Background(), tenantID)
			c := testCipher(t)
			var fleetID int64
			if tt.legacy {
				fleetID = insertPlainPluginFleet(t, ctx, pool, tenantID, projectID)
			} else {
				fleetID = createPluginFleet(t, ctx, pool, c, tenantID, projectID, sealMarker).ID
			}
			backend := &recordingBackend{fakeBackend: fakeBackend{name: "plugin:x"}}
			mgr := fleet.NewManager(fleet.NewPostgresStore(pool), fleet.NewPostgresFleetStore(pool, c), backend, fleet.ManagerOptions{
				Clock: func(int) time.Duration { return 0 },
			})

			_, err := mgr.Allocate(ctx, fleet.AllocationRequest{
				TenantID: tenantID, ProjectID: projectID, FleetID: fleetID, Region: "local", Capacity: 1,
			})

			require.NoError(t, err)
			assert.Equal(t, sealMarker, backend.got["api_token"])
		})
	}
}

func TestUpdateFleet_should_seal_an_old_plain_row_on_a_name_change(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)
	c := testCipher(t)
	id := insertPlainPluginFleet(t, ctx, pool, tenantID, projectID)
	name := "renamed"

	updated, err := fleet.NewPostgresFleetStore(pool, c).UpdateFleet(ctx, tenantID, projectID, id, &name, nil, actor)

	require.NoError(t, err)
	assert.NotContains(t, storedConfig(t, ctx, pool, id), sealMarker)
	assert.Equal(t, sealMarker, updated.Config["api_token"])
}

func TestUpdateFleet_should_refuse_a_name_change_when_a_plugin_value_does_not_open(t *testing.T) {
	pool, tenantID, projectID := startMigratedDB(t)
	ctx := db.WithTenant(context.Background(), tenantID)
	f := createPluginFleet(t, ctx, pool, testCipher(t), tenantID, projectID, sealMarker)
	before := storedConfig(t, ctx, pool, f.ID)
	name := "renamed"

	_, err := fleet.NewPostgresFleetStore(pool, testCipher(t)).UpdateFleet(ctx, tenantID, projectID, f.ID, &name, nil, actor)

	assert.ErrorIs(t, err, fleet.ErrConfigNotOpened)
	assert.Equal(t, before, storedConfig(t, ctx, pool, f.ID))
}
