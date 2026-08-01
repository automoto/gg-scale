//go:build integration

package secretseal_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/secretseal"
)

func startSecretSealDB(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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
	r, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, r.Up())
	require.NoError(t, r.Close())

	raw, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	return db.NewPool(raw), raw
}

func TestLoad_generates_and_persists_credential_key(t *testing.T) {
	pool, raw := startSecretSealDB(t)
	ctx := context.Background()

	first, err := secretseal.Load(ctx, pool, "")
	require.NoError(t, err)
	require.NotNil(t, first)

	var n int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM server_secrets WHERE name = 'credential_enc_key'`).Scan(&n))
	require.Equal(t, int64(1), n)

	sealed, err := first.Encrypt([]byte("steam-key"))
	require.NoError(t, err)

	// A second boot converges on the same key.
	second, err := secretseal.Load(ctx, pool, "")
	require.NoError(t, err)
	plaintext, err := second.Decrypt(sealed)
	require.NoError(t, err)
	assert.Equal(t, "steam-key", string(plaintext))
}
