//go:build integration

package jobs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/jobs"
	"github.com/ggscale/ggscale/internal/mailer"
)

func TestSendPasswordResetEmail_delivers_for_known_account_only(t *testing.T) {
	pool, raw := startJobsDB(t)
	ctx := context.Background()
	rec := &mailer.Recorder{}
	deps := jobs.PasswordResetDeps{
		Pool: pool, Mailer: rec, MailFrom: "no-reply@example.test",
		BaseURL: "http://app.example.test",
	}

	_, err := raw.Exec(ctx, `
INSERT INTO player_accounts (email, password_hash, email_verified_at)
VALUES ('known@example.test', '\x00'::bytea, now())`)
	require.NoError(t, err)

	// Unknown address: silent no-op, no email, no token row.
	require.NoError(t, jobs.SendPasswordResetEmail(ctx, deps,
		jobs.PasswordResetSurfacePlayerAccount, "unknown@example.test"))
	assert.Empty(t, rec.Snapshot())

	// Known address: token row + email carrying the reset link.
	require.NoError(t, jobs.SendPasswordResetEmail(ctx, deps,
		jobs.PasswordResetSurfacePlayerAccount, "known@example.test"))
	sent := rec.Snapshot()
	require.Len(t, sent, 1)
	assert.True(t, strings.Contains(sent[0].Body, "/v1/players/account/reset-password?token="),
		"email must carry the reset link: %q", sent[0].Body)
	var tokens int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM player_account_password_resets`).Scan(&tokens))
	assert.Equal(t, int64(1), tokens)
}

func TestSweepExpiredPasswordResets_removes_only_long_expired_rows(t *testing.T) {
	pool, raw := startJobsDB(t)
	ctx := context.Background()

	_, err := raw.Exec(ctx, `
INSERT INTO player_accounts (id, email, password_hash)
VALUES ('11111111-1111-1111-1111-111111111111', 'sweep@example.test', '\x00'::bytea);
INSERT INTO control_panel_users (id, email, password_hash)
VALUES (9101, 'sweep-cp@example.test', '\x00'::bytea);
INSERT INTO player_account_password_resets (player_account_id, token_hash, expires_at)
VALUES
    ('11111111-1111-1111-1111-111111111111', '\x01'::bytea, now() - interval '2 days'),
    ('11111111-1111-1111-1111-111111111111', '\x02'::bytea, now() + interval '1 hour');
INSERT INTO control_panel_password_resets (control_panel_user_id, token_hash, expires_at)
VALUES
    (9101, '\x03'::bytea, now() - interval '2 days'),
    (9101, '\x04'::bytea, now() + interval '1 hour')`)
	require.NoError(t, err)

	require.NoError(t, jobs.SweepExpiredPasswordResets(ctx, pool))

	var players, controlPanel int64
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM player_account_password_resets`).Scan(&players))
	require.NoError(t, raw.QueryRow(ctx,
		`SELECT count(*) FROM control_panel_password_resets`).Scan(&controlPanel))
	assert.Equal(t, int64(1), players, "only the long-expired player token is swept")
	assert.Equal(t, int64(1), controlPanel, "only the long-expired dashboard token is swept")
}
