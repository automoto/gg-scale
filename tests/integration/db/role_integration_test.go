//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
)

func TestAssertAppRoleHandlesTenantTableOwnerSessionByEnvironment(t *testing.T) {
	ctx := context.Background()
	ownerPool := startMigrated(t)
	poolConfig := ownerPool.Config()
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	t.Run("production rejects owner login", func(t *testing.T) {
		err := db.AssertAppRole(ctx, pool, true)

		assert.ErrorContains(t, err, "session_user \"ggscale\" can assume a tenant-table owner role")
	})

	t.Run("development permits owner fallback", func(t *testing.T) {
		err := db.AssertAppRole(ctx, pool, false)

		assert.NoError(t, err)
	})
}
