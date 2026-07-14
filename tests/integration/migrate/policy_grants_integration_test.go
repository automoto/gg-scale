//go:build integration

package migrate_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/rbac"
)

// TestMigrations_seed_casbin_p_policy_matches_code guards against the class of
// bug where the persisted Casbin policy drifts from the policy compiled into
// the binary (e.g. an applied migration edited in place). The migration seed
// must reproduce exactly the p-rules the in-memory authorizer loads from code.
func TestMigrations_seed_casbin_p_policy_matches_code(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	authorizer, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	rules, err := authorizer.PolicyRules()
	require.NoError(t, err)
	want := make([]string, 0, len(rules))
	for _, rule := range rules {
		want = append(want, strings.Join(rule, "|"))
	}

	dbc := openDB(t, dsn)
	rows, err := dbc.Query(
		`SELECT COALESCE(v0,''), COALESCE(v1,''), COALESCE(v2,''), COALESCE(v3,'')
		 FROM casbin_rule WHERE ptype = 'p'`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var got []string
	for rows.Next() {
		var v0, v1, v2, v3 string
		require.NoError(t, rows.Scan(&v0, &v1, &v2, &v3))
		got = append(got, strings.Join([]string{v0, v1, v2, v3}, "|"))
	}
	require.NoError(t, rows.Err())

	assert.ElementsMatch(t, want, got,
		"persisted casbin p-policy has drifted from rbac.defaultPolicyCSV; add a migration to reconcile it")
}

// TestMigrations_feature_grants_check_allows_every_code_feature guards the
// feature_grants_feature_check constraint against drifting from the Feature
// constants compiled into the binary. A feature the constraint rejects can
// never be deprovisioned: default-on features (matchmaker) are turned off by
// an explicit enabled=false row, which the constraint must accept.
func TestMigrations_feature_grants_check_allows_every_code_feature(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	dbc := openDB(t, dsn)
	var tenantID int64
	require.NoError(t, dbc.QueryRow(
		`INSERT INTO tenants (name, tier) VALUES ('feature-check', 'free') RETURNING id`).Scan(&tenantID))

	features := []rbac.Feature{
		rbac.FeatureP2PRelay,
		rbac.FeatureDedicatedServers,
		rbac.FeatureFleetDockerBackend,
		rbac.FeatureFleetAgonesBackend,
		rbac.FeatureFleetPluginBackend,
		rbac.FeatureMatchmaker,
	}
	for _, feature := range features {
		_, err := dbc.Exec(
			`INSERT INTO feature_grants (tenant_id, feature, enabled, reason)
			 VALUES ($1, $2, false, 'constraint guard')`, tenantID, string(feature))
		assert.NoError(t, err, "feature %q must satisfy feature_grants_feature_check", feature)
	}
}

// TestMigrations_grant_ggscale_app_dml_on_worker_tables guards the app-role
// grants the background workers depend on. A missing grant surfaces at runtime
// as "permission denied for table ..." rather than a test failure, so assert
// it explicitly on the tables whose grants have regressed before.
func TestMigrations_grant_ggscale_app_dml_on_worker_tables(t *testing.T) {
	dsn := startPostgres(t)

	r, err := migrate.New(dsn, migrationsDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Up())

	dbc := openDB(t, dsn)
	for _, table := range []string{"matchmaking_tickets", "game_server_allocations"} {
		for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
			var granted bool
			err := dbc.QueryRow(
				`SELECT EXISTS (
					SELECT 1 FROM information_schema.role_table_grants
					WHERE grantee = 'ggscale_app' AND table_name = $1 AND privilege_type = $2
				)`, table, priv).Scan(&granted)
			require.NoError(t, err)
			assert.True(t, granted, "ggscale_app should hold %s on %s", priv, table)
		}
	}
}
