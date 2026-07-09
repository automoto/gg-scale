//go:build integration

package auth_test

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

	"github.com/ggscale/ggscale/internal/auth"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/migrate"
)

func startAuthLoadDB(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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

func storedJWTKeyCount(t *testing.T, raw *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	require.NoError(t, raw.QueryRow(context.Background(),
		`SELECT count(*) FROM server_secrets WHERE name = 'jwt_signing_key'`).Scan(&n))
	return n
}

func signTestToken(t *testing.T, s *auth.Signer) string {
	t.Helper()
	tok, err := s.Sign(auth.Claims{PlayerID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	return tok
}

func TestLoad_generates_and_persists_key_when_nothing_configured(t *testing.T) {
	pool, raw := startAuthLoadDB(t)
	ctx := context.Background()

	first, err := auth.Load(ctx, pool, "")
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, int64(1), storedJWTKeyCount(t, raw))

	tok := signTestToken(t, first)

	// A second boot loads the same key: tokens minted before the restart
	// still verify, and no second row appears.
	second, err := auth.Load(ctx, pool, "")
	require.NoError(t, err)
	got, err := second.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.PlayerID)
	assert.Equal(t, int64(1), storedJWTKeyCount(t, raw))
}

func TestLoad_env_only_never_writes_a_database_key(t *testing.T) {
	pool, raw := startAuthLoadDB(t)
	envHex := strings.Repeat("ab", 32)

	s, err := auth.Load(context.Background(), pool, envHex)

	require.NoError(t, err)
	require.NotNil(t, s)
	assert.Equal(t, int64(0), storedJWTKeyCount(t, raw))
	// Same key material as a signer built straight from the env value.
	plain, err := auth.NewSignerFromHex(envHex)
	require.NoError(t, err)
	_, err = plain.Verify(signTestToken(t, s))
	assert.NoError(t, err)
}

func TestLoad_env_key_wins_over_database_key(t *testing.T) {
	pool, _ := startAuthLoadDB(t)
	ctx := context.Background()
	envHex := strings.Repeat("ab", 32)

	dbOnly, err := auth.Load(ctx, pool, "")
	require.NoError(t, err)
	dbToken := signTestToken(t, dbOnly)

	envLoaded, err := auth.Load(ctx, pool, envHex)
	require.NoError(t, err)

	// The env key is primary and exclusive: tokens minted under the
	// database key stop verifying after the switch (clients recover via
	// the refresh flow).
	_, err = envLoaded.Verify(dbToken)
	assert.Error(t, err)
	_, err = envLoaded.Verify(signTestToken(t, envLoaded))
	assert.NoError(t, err)
}

func TestLoad_rejects_malformed_env_key(t *testing.T) {
	pool, _ := startAuthLoadDB(t)

	cases := []struct {
		name   string
		envHex string
	}{
		{name: "not_hex", envHex: "not-hex"},
		{name: "too_short", envHex: "deadbeef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.Load(context.Background(), pool, tc.envHex)

			assert.Error(t, err)
		})
	}
}
