//go:build integration

package controlpanel

import (
	"context"
	"errors"
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
)

// startLeaderboardDB brings up a migrated Postgres with one tenant and two
// projects, returning a Handler wired to the pool plus the two project ids.
func startLeaderboardDB(t *testing.T) (*Handler, int64, int64, int64) {
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

	var tenantID, projectA, projectB int64
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ('lb-test') RETURNING id`).Scan(&tenantID))
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p1') RETURNING id`, tenantID).Scan(&projectA))
	require.NoError(t, raw.QueryRow(ctx,
		`INSERT INTO projects (tenant_id, name) VALUES ($1, 'p2') RETURNING id`, tenantID).Scan(&projectB))
	return &Handler{pool: pool}, tenantID, projectA, projectB
}

func TestLeaderboardStore_create_list_and_sort_order_roundtrip(t *testing.T) {
	h, tenantID, projectA, _ := startLeaderboardDB(t)
	ctx := context.Background()

	// Empty string defaults to desc via the handler layer; pass it through create.
	descID, err := h.createLeaderboard(ctx, tenantID, projectA, "weekly", sortOrderDesc)
	require.NoError(t, err)
	require.NotZero(t, descID)
	ascID, err := h.createLeaderboard(ctx, tenantID, projectA, "fastest-lap", sortOrderAsc)
	require.NoError(t, err)

	rows, err := h.listLeaderboards(ctx, tenantID, projectA)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	byName := map[string]LeaderboardRowView{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	assert.Equal(t, "desc", byName["weekly"].SortOrder)
	assert.Equal(t, "asc", byName["fastest-lap"].SortOrder)
	assert.Equal(t, ascID, byName["fastest-lap"].ID)
}

func TestLeaderboardStore_get_is_project_scoped(t *testing.T) {
	h, tenantID, projectA, projectB := startLeaderboardDB(t)
	ctx := context.Background()

	id, err := h.createLeaderboard(ctx, tenantID, projectA, "board", sortOrderDesc)
	require.NoError(t, err)

	got, err := h.getLeaderboard(ctx, tenantID, projectA, id)
	require.NoError(t, err)
	assert.Equal(t, "board", got.Name)

	// Same id under the sibling project must not resolve.
	_, err = h.getLeaderboard(ctx, tenantID, projectB, id)
	assert.True(t, errors.Is(err, pgx.ErrNoRows))
}

func TestLeaderboardStore_update_changes_name_and_sort_order(t *testing.T) {
	h, tenantID, projectA, _ := startLeaderboardDB(t)
	ctx := context.Background()

	id, err := h.createLeaderboard(ctx, tenantID, projectA, "board", sortOrderDesc)
	require.NoError(t, err)

	require.NoError(t, h.updateLeaderboard(ctx, tenantID, projectA, id, "renamed", sortOrderAsc))

	got, err := h.getLeaderboard(ctx, tenantID, projectA, id)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "asc", got.SortOrder)
}

func TestLeaderboardStore_update_missing_row_returns_no_rows(t *testing.T) {
	h, tenantID, projectA, _ := startLeaderboardDB(t)
	ctx := context.Background()

	id, err := h.createLeaderboard(ctx, tenantID, projectA, "board", sortOrderDesc)
	require.NoError(t, err)
	require.NoError(t, h.softDeleteLeaderboard(ctx, tenantID, projectA, id))

	// Updating a deleted (or never-existing) leaderboard must surface as
	// not-found, not silent success — the handler reports it as a 404.
	err = h.updateLeaderboard(ctx, tenantID, projectA, id, "renamed", sortOrderAsc)
	assert.True(t, errors.Is(err, pgx.ErrNoRows))
	err = h.softDeleteLeaderboard(ctx, tenantID, projectA, id)
	assert.True(t, errors.Is(err, pgx.ErrNoRows))
}

func TestLeaderboardStore_soft_delete_hides_and_frees_name(t *testing.T) {
	h, tenantID, projectA, _ := startLeaderboardDB(t)
	ctx := context.Background()

	id, err := h.createLeaderboard(ctx, tenantID, projectA, "board", sortOrderDesc)
	require.NoError(t, err)
	require.NoError(t, h.softDeleteLeaderboard(ctx, tenantID, projectA, id))

	rows, err := h.listLeaderboards(ctx, tenantID, projectA)
	require.NoError(t, err)
	assert.Empty(t, rows)

	// The partial unique index only covers live rows, so the name is reusable.
	_, err = h.createLeaderboard(ctx, tenantID, projectA, "board", sortOrderDesc)
	require.NoError(t, err)
}

func TestLeaderboardStore_duplicate_name_on_create_and_rename(t *testing.T) {
	h, tenantID, projectA, _ := startLeaderboardDB(t)
	ctx := context.Background()

	_, err := h.createLeaderboard(ctx, tenantID, projectA, "board", sortOrderDesc)
	require.NoError(t, err)
	_, err = h.createLeaderboard(ctx, tenantID, projectA, "board", sortOrderDesc)
	assert.True(t, errors.Is(err, errDuplicateLeaderboard), "duplicate create should map to errDuplicateLeaderboard")

	other, err := h.createLeaderboard(ctx, tenantID, projectA, "other", sortOrderDesc)
	require.NoError(t, err)
	err = h.updateLeaderboard(ctx, tenantID, projectA, other, "board", sortOrderDesc)
	assert.True(t, errors.Is(err, errDuplicateLeaderboard), "rename collision should map to errDuplicateLeaderboard")
}
