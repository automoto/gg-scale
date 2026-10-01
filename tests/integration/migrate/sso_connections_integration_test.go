//go:build integration

// e2e:bucket b

package migrate_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/migrate"
)

const ssoConnectionsDown = "0048_sso_connections.down.sql"

func TestSSOConnections_down_refuses_when_an_account_has_no_password(t *testing.T) {
	tests := []struct {
		name   string
		insert string
	}{
		{"player account", `INSERT INTO player_accounts (email, email_verified_at) VALUES ('p@example.com', now())`},
		{"control panel user", `INSERT INTO control_panel_users (email, email_verified_at) VALUES ('u@example.com', now())`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dsn := startPostgres(t)
			r, err := migrate.New(dsn, migrationsDir(t))
			require.NoError(t, err)
			t.Cleanup(func() { _ = r.Close() })
			require.NoError(t, r.Up())
			dbc := openDB(t, dsn)
			_, err = dbc.Exec(tc.insert)
			require.NoError(t, err)
			down, err := os.ReadFile(filepath.Join(migrationsDir(t), ssoConnectionsDown))
			require.NoError(t, err)

			_, err = dbc.Exec(string(down))

			require.ErrorContains(t, err, "accounts without a password exist")
			assert.True(t, tableExists(t, dsn, "player_account_connections"))
		})
	}
}

func TestSSOConnections_down_restores_the_password_requirement(t *testing.T) {
	dsn := startPostgres(t)
	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())
	dbc := openDB(t, dsn)

	execMigrationFile(t, dbc, ssoConnectionsDown)

	_, err = dbc.Exec(`INSERT INTO player_accounts (email) VALUES ('p@example.com')`)
	assert.ErrorContains(t, err, "password_hash")
	assert.False(t, tableExists(t, dsn, "control_panel_user_connections"))
}
