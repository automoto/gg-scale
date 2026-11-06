//go:build integration

package jobs_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/jobs"
)

func seedSignupRequest(t *testing.T, raw *pgxpool.Pool, name, status, expiresIn string) {
	t.Helper()
	_, err := raw.Exec(context.Background(),
		`INSERT INTO tenant_signup_requests
		   (email, requested_tenant_name, project_description, status, code_hash, code_expires_at)
		 VALUES ($1 || '@example.com', $1, 'A game.', $2, $1::bytea, now() + $3::interval)`,
		name, status, expiresIn)
	require.NoError(t, err)
}

func signupStatuses(t *testing.T, raw *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := raw.Query(context.Background(), `SELECT requested_tenant_name, status FROM tenant_signup_requests`)
	require.NoError(t, err)
	defer rows.Close()
	statuses := map[string]string{}
	for rows.Next() {
		var name, status string
		require.NoError(t, rows.Scan(&name, &status))
		statuses[name] = status
	}
	require.NoError(t, rows.Err())
	return statuses
}

func TestSweepExpiredTenantSignups_should_delete_only_expired_approvals(t *testing.T) {
	pool, raw := startJobsDB(t)
	seedSignupRequest(t, raw, "expired", "approved", "-1 hour")
	seedSignupRequest(t, raw, "live", "approved", "1 hour")
	seedSignupRequest(t, raw, "pending", "pending", "-1 hour")
	seedSignupRequest(t, raw, "denied", "denied", "-1 hour")
	seedSignupRequest(t, raw, "accepted", "accepted", "-1 hour")

	require.NoError(t, jobs.SweepExpiredTenantSignups(context.Background(), pool))

	assert.Equal(t, map[string]string{
		"live":     "approved",
		"pending":  "pending",
		"denied":   "denied",
		"accepted": "accepted",
	}, signupStatuses(t, raw))
}

func TestSweepExpiredTenantSignups_should_release_the_name_and_the_email(t *testing.T) {
	pool, raw := startJobsDB(t)
	ctx := context.Background()
	seedSignupRequest(t, raw, "wanted", "approved", "-1 hour")

	require.NoError(t, jobs.SweepExpiredTenantSignups(ctx, pool))

	_, err := raw.Exec(ctx,
		`INSERT INTO tenant_signup_requests (email, requested_tenant_name, project_description)
		 VALUES ('wanted@example.com', 'wanted', 'A game.')`)
	assert.NoError(t, err)
}
