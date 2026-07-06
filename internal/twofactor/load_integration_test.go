//go:build integration

package twofactor

import (
	"context"
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
)

func startTwoFactorLoadDB(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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
	return db.NewPool(raw), raw
}

func storedKeyCount(t *testing.T, raw *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`SELECT count(*) FROM server_secrets WHERE name = 'two_factor_enc_key'`).Scan(&n))
	return n
}

func TestLoad_generates_and_persists_key_when_nothing_configured(t *testing.T) {
	pool, raw := startTwoFactorLoadDB(t)
	ctx := context.Background()

	first, err := Load(ctx, pool, "")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, int64(1), storedKeyCount(t, raw))

	sealed, err := first.Encrypt([]byte("JBSWY3DPEHPK3PXP"))
	require.NoError(t, err)

	// A second boot loads the same key: old ciphertext still opens, no
	// second row appears.
	second, err := Load(ctx, pool, "")
	require.NoError(t, err)
	plaintext, err := second.Decrypt(sealed)
	require.NoError(t, err)
	assert.Equal(t, "JBSWY3DPEHPK3PXP", string(plaintext))
	assert.Equal(t, int64(1), storedKeyCount(t, raw))
}

func TestLoad_env_only_never_writes_a_database_key(t *testing.T) {
	pool, raw := startTwoFactorLoadDB(t)
	envHex := strings.Repeat("ab", 32)

	c, err := Load(context.Background(), pool, envHex)

	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, int64(0), storedKeyCount(t, raw))
	// Same key material as a plain env cipher: pending cookies interop.
	plain, err := NewCipher(envHex)
	require.NoError(t, err)
	assert.Equal(t, plain.PendingKey(), c.PendingKey())
}

func TestLoad_env_key_keeps_database_key_as_decrypt_fallback(t *testing.T) {
	pool, _ := startTwoFactorLoadDB(t)
	ctx := context.Background()
	envHex := strings.Repeat("ab", 32)

	dbOnly, err := Load(ctx, pool, "")
	require.NoError(t, err)
	oldSealed, err := dbOnly.Encrypt([]byte("old-enrollment"))
	require.NoError(t, err)

	ring, err := Load(ctx, pool, envHex)
	require.NoError(t, err)

	// Old enrollment still opens after the operator switches to the env key.
	plaintext, err := ring.Decrypt(oldSealed)
	require.NoError(t, err)
	assert.Equal(t, "old-enrollment", string(plaintext))

	// New secrets seal under the env key: an env-only cipher opens them,
	// the old db-only cipher does not.
	newSealed, err := ring.Encrypt([]byte("new-enrollment"))
	require.NoError(t, err)
	envOnly, err := NewCipher(envHex)
	require.NoError(t, err)
	_, err = envOnly.Decrypt(newSealed)
	assert.NoError(t, err)
	_, err = dbOnly.Decrypt(newSealed)
	assert.Error(t, err)
}

func TestLoad_rejects_malformed_env_key(t *testing.T) {
	pool, _ := startTwoFactorLoadDB(t)

	_, err := Load(context.Background(), pool, "not-hex")

	assert.Error(t, err)
}
