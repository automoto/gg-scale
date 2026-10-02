//go:build integration

package projectadmin_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/migrate"
	"github.com/automoto/gg-scale/internal/projectadmin"
)

type fixture struct {
	pool     *db.Pool
	owner    *pgxpool.Pool
	tenantID int64
	project  int64
	other    int64
	user     projectadmin.Actor
}

// newFixture migrates a fresh Postgres and returns an app-role pool, so row
// security and the ggscale_app grants apply as they do in production.
func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17",
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
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "db", "migrations"))
	require.NoError(t, err)
	runner, err := migrate.New(dsn, dir)
	require.NoError(t, err)
	require.NoError(t, runner.Up())
	require.NoError(t, runner.Close())

	owner, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(owner.Close)

	f := fixture{owner: owner}
	require.NoError(t, owner.QueryRow(ctx, `INSERT INTO tenants (name) VALUES ('pa') RETURNING id`).Scan(&f.tenantID))
	require.NoError(t, owner.QueryRow(ctx, `INSERT INTO projects (tenant_id, name) VALUES ($1, 'p1') RETURNING id`, f.tenantID).Scan(&f.project))
	require.NoError(t, owner.QueryRow(ctx, `INSERT INTO projects (tenant_id, name) VALUES ($1, 'p2') RETURNING id`, f.tenantID).Scan(&f.other))
	require.NoError(t, owner.QueryRow(ctx,
		`INSERT INTO control_panel_users (email, password_hash, email_verified_at)
		 VALUES ('pa@example.com', '\x00'::bytea, now()) RETURNING id`).Scan(&f.user.UserID))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(app.Close)
	f.pool = db.NewPool(app)
	return f
}

func (f fixture) revisions(t *testing.T, kind string, resourceID int64) []projectadmin.Revision {
	t.Helper()
	revs, err := projectadmin.ListRevisions(context.Background(), f.pool, f.tenantID, f.project, kind, resourceID)
	require.NoError(t, err)
	return revs
}

func revisionNumbers(revs []projectadmin.Revision) []int64 {
	out := make([]int64, 0, len(revs))
	for _, r := range revs {
		out = append(out, r.Revision)
	}
	return out
}

func (f fixture) setConfig(t *testing.T, config string) int64 {
	t.Helper()
	rev, err := projectadmin.SetRemoteConfig(context.Background(), f.pool, f.tenantID, f.project, []byte(config), nil, f.user)
	require.NoError(t, err)
	return rev
}

func (f fixture) remoteConfig(t *testing.T) string {
	t.Helper()
	var out string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT remote_config::text FROM projects WHERE id = $1`, f.project).Scan(&out))
	return out
}

func (f fixture) createBoard(t *testing.T, name string) int64 {
	t.Helper()
	id, err := projectadmin.CreateLeaderboard(context.Background(), f.pool, f.tenantID, f.project, board(name), f.user)
	require.NoError(t, err)
	return id
}

func board(name string) projectadmin.LeaderboardSettings {
	return projectadmin.LeaderboardSettings{
		Name: name, SortOrder: "desc", ScoreOperator: "best", ResetSchedule: "none",
	}
}

func (f fixture) auditCount(t *testing.T, action string) int {
	t.Helper()
	var n int
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT count(*) FROM platform_audit_log WHERE action = $1`, action).Scan(&n))
	return n
}

func TestRemoteConfig_should_store_base_row_on_first_write(t *testing.T) {
	// Arrange
	f := newFixture(t)

	// Act
	f.setConfig(t, `{"a":1}`)

	// Assert
	revs := f.revisions(t, projectadmin.KindRemoteConfig, f.project)
	require.Len(t, revs, 2)
	assert.JSONEq(t, `{}`, string(revs[1].Snapshot))
}

func TestRemoteConfig_should_increase_revision_on_each_write(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"a":1}`)

	rev := f.setConfig(t, `{"a":2}`)

	assert.Equal(t, int64(3), rev)
}

func TestRemoteConfig_should_keep_newest_three_revisions(t *testing.T) {
	f := newFixture(t)
	for _, c := range []string{`{"a":1}`, `{"a":2}`, `{"a":3}`, `{"a":4}`} {
		f.setConfig(t, c)
	}

	revs := f.revisions(t, projectadmin.KindRemoteConfig, f.project)

	assert.Equal(t, []int64{5, 4, 3}, revisionNumbers(revs))
}

func TestRemoteConfig_should_refuse_stale_expected_revision(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"a":1}`)
	stale := int64(1)

	_, err := projectadmin.SetRemoteConfig(context.Background(), f.pool, f.tenantID, f.project, []byte(`{"a":2}`), &stale, f.user)

	assert.ErrorIs(t, err, projectadmin.ErrRevisionConflict)
}

func TestRemoteConfig_should_write_audit_row(t *testing.T) {
	f := newFixture(t)

	f.setConfig(t, `{"a":1}`)

	assert.Equal(t, 1, f.auditCount(t, "control_panel.remote_config.update"))
}

func TestRemoteConfig_rollback_should_apply_kept_snapshot_as_new_revision(t *testing.T) {
	f := newFixture(t)
	f.setConfig(t, `{"a":1}`)
	f.setConfig(t, `{"a":2}`)

	rev, err := projectadmin.RollbackRemoteConfig(context.Background(), f.pool, f.tenantID, f.project, 2, nil, f.user)
	require.NoError(t, err)

	assert.Equal(t, int64(4), rev)
	assert.JSONEq(t, `{"a":1}`, f.remoteConfig(t))
	assert.Equal(t, "rollback", f.revisions(t, projectadmin.KindRemoteConfig, f.project)[0].Source)
}

func TestRemoteConfig_rollback_should_refuse_pruned_revision(t *testing.T) {
	f := newFixture(t)
	for _, c := range []string{`{"a":1}`, `{"a":2}`, `{"a":3}`} {
		f.setConfig(t, c)
	}

	_, err := projectadmin.RollbackRemoteConfig(context.Background(), f.pool, f.tenantID, f.project, 1, nil, f.user)

	assert.ErrorIs(t, err, projectadmin.ErrRevisionNotFound)
}

func TestRemoteConfig_token_write_should_record_token_as_actor(t *testing.T) {
	f := newFixture(t)
	var tokenID int64
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`INSERT INTO mcp_tokens (tenant_id, project_id, created_by_user_id, label, token_hash, token_hint, expires_at)
		 VALUES ($1, $2, $3, 't', '\x01'::bytea, 'hint', now() + interval '1 day') RETURNING id`,
		f.tenantID, f.project, f.user.UserID).Scan(&tokenID))
	token := projectadmin.Actor{TokenID: tokenID, TokenCreatorID: f.user.UserID}

	_, err := projectadmin.SetRemoteConfig(context.Background(), f.pool, f.tenantID, f.project, []byte(`{"a":1}`), nil, token)
	require.NoError(t, err)

	var service string
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT actor_service FROM platform_audit_log WHERE action = 'control_panel.remote_config.update'`).Scan(&service))
	assert.Equal(t, "mcp_token:"+strconv.FormatInt(tokenID, 10), service)
	newest := f.revisions(t, projectadmin.KindRemoteConfig, f.project)[0]
	assert.Equal(t, []any{"mcp", tokenID}, []any{newest.Source, *newest.TokenID})
}

func TestLeaderboard_create_should_record_revision_one_without_base_row(t *testing.T) {
	f := newFixture(t)

	id := f.createBoard(t, "scores")

	assert.Equal(t, []int64{1}, revisionNumbers(f.revisions(t, projectadmin.KindLeaderboard, id)))
}

func TestLeaderboard_update_should_record_revision(t *testing.T) {
	f := newFixture(t)
	id := f.createBoard(t, "scores")
	s := board("renamed")

	rev, err := projectadmin.UpdateLeaderboard(context.Background(), f.pool, f.tenantID, f.project, id, s, nil, f.user)
	require.NoError(t, err)

	assert.Equal(t, int64(2), rev)
}

func TestLeaderboard_update_should_refuse_stale_expected_revision(t *testing.T) {
	f := newFixture(t)
	id := f.createBoard(t, "scores")
	stale := int64(0)

	_, err := projectadmin.UpdateLeaderboard(context.Background(), f.pool, f.tenantID, f.project, id, board("x"), &stale, f.user)

	assert.ErrorIs(t, err, projectadmin.ErrRevisionConflict)
}

func TestLeaderboard_rollback_should_restore_settings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.createBoard(t, "scores")
	_, err := projectadmin.UpdateLeaderboard(ctx, f.pool, f.tenantID, f.project, id, board("renamed"), nil, f.user)
	require.NoError(t, err)

	_, err = projectadmin.RollbackLeaderboard(ctx, f.pool, f.tenantID, f.project, id, 1, nil, f.user)
	require.NoError(t, err)

	var name string
	require.NoError(t, f.owner.QueryRow(ctx, `SELECT name FROM leaderboards WHERE id = $1`, id).Scan(&name))
	assert.Equal(t, "scores", name)
}

func TestLeaderboard_revisions_should_be_project_scoped(t *testing.T) {
	f := newFixture(t)
	id := f.createBoard(t, "scores")

	revs, err := projectadmin.ListRevisions(context.Background(), f.pool, f.tenantID, f.other, projectadmin.KindLeaderboard, id)
	require.NoError(t, err)

	assert.Empty(t, revs)
}

func TestLeaderboard_restore_should_clear_deleted_at(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.createBoard(t, "scores")
	require.NoError(t, projectadmin.DeleteLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user))

	require.NoError(t, projectadmin.RestoreLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user))

	assert.Equal(t, 1, f.auditCount(t, "leaderboard.restore"))
}

func TestLeaderboard_restore_should_fail_when_name_is_in_use(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.createBoard(t, "scores")
	require.NoError(t, projectadmin.DeleteLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user))
	f.createBoard(t, "scores")

	err := projectadmin.RestoreLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user)

	assert.ErrorIs(t, err, projectadmin.ErrDuplicateLeaderboard)
}

func TestLeaderboard_restore_should_not_find_board_of_other_project(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.createBoard(t, "scores")
	require.NoError(t, projectadmin.DeleteLeaderboard(ctx, f.pool, f.tenantID, f.project, id, f.user))

	err := projectadmin.RestoreLeaderboard(ctx, f.pool, f.tenantID, f.other, id, f.user)

	assert.ErrorIs(t, err, pgx.ErrNoRows)
}
