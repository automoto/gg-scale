//go:build integration

package verifycode_test

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/signedcookie"
	"github.com/ggscale/ggscale/internal/verifycode"
)

func startSigningKeyDB(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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
	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "..", "db", "migrations"))
	require.NoError(t, err)
	runner, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, runner.Up())
	require.NoError(t, runner.Close())

	raw, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	return db.NewPool(raw), raw
}

func storedSigningKeyCount(t *testing.T, raw *pgxpool.Pool) int64 {
	t.Helper()
	var count int64
	require.NoError(t, raw.QueryRow(context.Background(), `
SELECT count(*)
FROM server_secrets
WHERE name = 'email_verify_signing_key_v1'`).Scan(&count))
	return count
}

func TestLoadSigningKeyGeneratesOnePersistentSharedKey(t *testing.T) {
	pool, raw := startSigningKeyDB(t)
	ctx := context.Background()

	first, err := verifycode.LoadSigningKey(ctx, pool, "")
	require.NoError(t, err)
	second, err := verifycode.LoadSigningKey(ctx, pool, "")
	require.NoError(t, err)
	cookie := signedcookie.Sign(first, []byte("pending verification"))
	_, ok := signedcookie.Open(second, cookie)

	assert.True(t, ok)
	assert.Equal(t, int64(1), storedSigningKeyCount(t, raw))
}

func TestLoadSigningKeyEnvironmentOverrideWins(t *testing.T) {
	pool, raw := startSigningKeyDB(t)
	envHex := strings.Repeat("ab", 32)
	want, err := hex.DecodeString(envHex)
	require.NoError(t, err)
	databaseKey, err := verifycode.LoadSigningKey(context.Background(), pool, "")
	require.NoError(t, err)

	got, err := verifycode.LoadSigningKey(context.Background(), pool, envHex)

	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.NotEqual(t, databaseKey, got)

	_, err = raw.Exec(context.Background(), `DELETE FROM server_secrets WHERE name = 'email_verify_signing_key_v1'`)
	require.NoError(t, err)
	_, err = verifycode.LoadSigningKey(context.Background(), pool, envHex)
	require.NoError(t, err)
	assert.Zero(t, storedSigningKeyCount(t, raw))
}
