//go:build integration

package relaymeter

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/migrate"
	"github.com/ggscale/ggscale/internal/quota"
)

const (
	enforcedTenant   = int64(9301) // tier_0, enforce_quotas=true → allowance 1,000
	unenforcedTenant = int64(9302) // enforce_quotas=false → unmetered
)

type recorderMailer struct {
	mu   sync.Mutex
	msgs []mailer.Message
}

func (r *recorderMailer) Send(_ context.Context, msg mailer.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
	return nil
}

func (r *recorderMailer) subjects() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	for i, m := range r.msgs {
		out[i] = m.Subject
	}
	return out
}

func startMeterPostgres(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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
	runner, err := migrate.New(dsn, migrationsDir)
	require.NoError(t, err)
	require.NoError(t, runner.Up())
	require.NoError(t, runner.Close())

	owner, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(owner.Close)
	_, err = owner.Exec(ctx, `
		INSERT INTO tenants (id, name, enforce_quotas) VALUES
			(9301, 'metered', true),
			(9302, 'unmetered', false);
		INSERT INTO control_panel_users (id, email, password_hash, email_verified_at)
		VALUES (9401, 'owner@example.com', '\x00'::bytea, now());
		INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role)
		VALUES (9401, 9301, 'owner');
	`)
	require.NoError(t, err)

	appConfig, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	appConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE ggscale_app")
		return err
	}
	appRaw, err := pgxpool.NewWithConfig(ctx, appConfig)
	require.NoError(t, err)
	t.Cleanup(appRaw.Close)
	return db.NewPool(appRaw), owner
}

func newTestMeter(t *testing.T, at time.Time) (*Meter, *pgxpool.Pool, *recorderMailer) {
	t.Helper()
	pool, owner := startMeterPostgres(t)
	mails := &recorderMailer{}
	m := New(pool, mails, "noreply@ggscale.dev")
	m.now = func() time.Time { return at }
	return m, owner, mails
}

// seedUsage pre-loads the month's counter so threshold tests don't need a
// thousand increments.
func seedUsage(t *testing.T, owner *pgxpool.Pool, tenantID int64, month time.Time, sessions int64) {
	t.Helper()
	_, err := owner.Exec(context.Background(),
		`INSERT INTO relay_session_usage (tenant_id, month, sessions) VALUES ($1, $2, $3)`,
		tenantID, month, sessions)
	require.NoError(t, err)
}

func TestAllow_unenforced_tenant_is_unmetered(t *testing.T) {
	m, _, mails := newTestMeter(t, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))

	for range 5 {
		require.NoError(t, m.Allow(context.Background(), unenforcedTenant))
	}
	assert.Empty(t, mails.subjects())
}

func TestAllow_warns_once_at_80_percent(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	m, owner, mails := newTestMeter(t, now)
	seedUsage(t, owner, enforcedTenant, monthStart(now), 799)

	require.NoError(t, m.Allow(context.Background(), enforcedTenant)) // 800 = 80% of 1,000
	require.NoError(t, m.Allow(context.Background(), enforcedTenant)) // 801: no second warning

	subjects := mails.subjects()
	require.Len(t, subjects, 1)
	assert.Contains(t, subjects[0], "80%")
}

func TestAllow_warns_at_100_percent_then_refuses(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	m, owner, mails := newTestMeter(t, now)
	seedUsage(t, owner, enforcedTenant, monthStart(now), 999)

	require.NoError(t, m.Allow(context.Background(), enforcedTenant)) // 1,000 = allowance

	err := m.Allow(context.Background(), enforcedTenant) // 1,001 refused
	var qe *quota.ErrQuotaExceeded
	require.ErrorAs(t, err, &qe)
	assert.Equal(t, quota.AxisRelaySessions, qe.Axis)
	assert.Equal(t, int64(1000), qe.Limit)

	// The refused attempt is not counted.
	var sessions int64
	require.NoError(t, owner.QueryRow(context.Background(),
		`SELECT sessions FROM relay_session_usage WHERE tenant_id = $1`, enforcedTenant).Scan(&sessions))
	assert.Equal(t, int64(1000), sessions)

	// Both thresholds were crossed by the same increment: 80% first (missed
	// earlier), then 100% — each warned exactly once.
	subjects := mails.subjects()
	require.Len(t, subjects, 2)
	assert.Contains(t, subjects[1], "allowance")
}

func TestAllow_new_month_resets_the_allowance(t *testing.T) {
	july := time.Date(2026, 7, 31, 23, 0, 0, 0, time.UTC)
	m, owner, _ := newTestMeter(t, july)
	seedUsage(t, owner, enforcedTenant, monthStart(july), 1000)

	var qe *quota.ErrQuotaExceeded
	require.ErrorAs(t, m.Allow(context.Background(), enforcedTenant), &qe, "july is exhausted")

	m.now = func() time.Time { return time.Date(2026, 8, 1, 0, 5, 0, 0, time.UTC) }
	assert.NoError(t, m.Allow(context.Background(), enforcedTenant), "august starts fresh")
}

func TestAllow_unknown_tenant_errors(t *testing.T) {
	m, _, _ := newTestMeter(t, time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC))

	err := m.Allow(context.Background(), 424242)
	require.Error(t, err)
	assert.False(t, errors.As(err, new(*quota.ErrQuotaExceeded)), "missing tenant is an internal error, not a quota rejection")
}
