//go:build integration

package entitlement

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func (r *recorderMailer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

type entitlementFixture struct {
	handler   http.Handler
	appPool   *db.Pool
	ownerPool *pgxpool.Pool
	mails     *recorderMailer
	reloads   *int
}

func startEntitlementPostgres(t *testing.T) (*db.Pool, *pgxpool.Pool) {
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
		INSERT INTO tenants (id, name) VALUES (9001, 'billing test');
		INSERT INTO control_panel_users (id, email, password_hash, email_verified_at)
		VALUES (9101, 'owner@example.com', '\x00'::bytea, now());
		INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role)
		VALUES (9101, 9001, 'owner');
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

func newEntitlementFixture(t *testing.T) entitlementFixture {
	t.Helper()
	appPool, owner := startEntitlementPostgres(t)
	mails := &recorderMailer{}
	reloads := 0
	h := New(Deps{
		Pool:     appPool,
		Mailer:   mails,
		MailFrom: "noreply@ggscale.dev",
		Token:    testToken,
		ReloadRBAC: func(context.Context) {
			reloads++
		},
	})
	return entitlementFixture{handler: h, appPool: appPool, ownerPool: owner, mails: mails, reloads: &reloads}
}

func (f entitlementFixture) put(t *testing.T, tenantID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, f.handler, http.MethodPut, "/"+tenantID, testToken, body)
}

func (f entitlementFixture) get(t *testing.T, tenantID string) (int, State) {
	t.Helper()
	rec := doRequest(t, f.handler, http.MethodGet, "/"+tenantID, testToken, "")
	var s State
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
	}
	return rec.Code, s
}

type auditRow struct {
	actorService *string
	actorUserID  *int64
	payload      string
}

func (f entitlementFixture) auditRows(t *testing.T) []auditRow {
	t.Helper()
	rows, err := f.ownerPool.Query(context.Background(),
		`SELECT actor_service, actor_user_id, payload::text FROM platform_audit_log WHERE action = 'entitlement.apply' ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		require.NoError(t, rows.Scan(&row.actorService, &row.actorUserID, &row.payload))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestEntitlementAPI_applies_tier_and_features(t *testing.T) {
	f := newEntitlementFixture(t)

	rec := f.put(t, "9001", `{"tier":2,"features":["p2p_relay"]}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"changed":true`)

	code, state := f.get(t, "9001")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 2, state.Tier)
	assert.Equal(t, []string{"p2p_relay"}, state.Features)

	rows := f.auditRows(t)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].actorService)
	assert.Equal(t, "billing-service", *rows[0].actorService)
	assert.Nil(t, rows[0].actorUserID, "service rows have no human actor")
	assert.Contains(t, rows[0].payload, `"new_tier": 2`)
	assert.Equal(t, 1, f.mails.count(), "one decision email for the change")
	assert.Equal(t, 1, *f.reloads, "rbac policy reloaded after the grant change")
}

func TestEntitlementAPI_apply_is_idempotent(t *testing.T) {
	f := newEntitlementFixture(t)

	first := f.put(t, "9001", `{"tier":2,"features":["p2p_relay"]}`)
	second := f.put(t, "9001", `{"tier":2,"features":["p2p_relay"]}`)

	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, http.StatusOK, second.Code)
	assert.Contains(t, second.Body.String(), `"changed":false`)
	assert.Len(t, f.auditRows(t), 1, "no duplicate audit entry")
	assert.Equal(t, 1, f.mails.count(), "no repeat email")
}

func TestEntitlementAPI_downgrade_disables_managed_features_without_dropping_rows(t *testing.T) {
	f := newEntitlementFixture(t)
	require.Equal(t, http.StatusOK, f.put(t, "9001", `{"tier":2,"features":["p2p_relay"]}`).Code)

	rec := f.put(t, "9001", `{"tier":0,"features":[]}`)

	require.Equal(t, http.StatusOK, rec.Code)
	code, state := f.get(t, "9001")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 0, state.Tier)
	assert.Empty(t, state.Features)

	var enabled bool
	err := f.ownerPool.QueryRow(context.Background(),
		`SELECT enabled FROM feature_grants WHERE tenant_id = 9001 AND project_id IS NULL AND feature = 'p2p_relay'`,
	).Scan(&enabled)
	require.NoError(t, err, "the grant row survives a downgrade (disabled, not deleted)")
	assert.False(t, enabled)
}

func TestEntitlementAPI_unknown_tenant_is_not_found(t *testing.T) {
	f := newEntitlementFixture(t)

	rec := f.put(t, "424242", `{"tier":1,"features":[]}`)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLoadToken_generates_once_and_env_wins(t *testing.T) {
	appPool, _ := startEntitlementPostgres(t)
	ctx := context.Background()

	generated, err := LoadToken(ctx, appPool, "")
	require.NoError(t, err)
	assert.Len(t, generated, 64, "32 random bytes, hex-encoded")

	again, err := LoadToken(ctx, appPool, "")
	require.NoError(t, err)
	assert.Equal(t, generated, again, "second boot converges on the stored token")

	pinned, err := LoadToken(ctx, appPool, strings.Repeat("b", 32))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("b", 32), pinned, "env token overrides the stored one")
}
