//go:build integration

// e2e:bucket a

package controlpanel_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/controlpanel"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/secretseal"
)

type teamOwner struct {
	client       *http.Client
	csrf         string
	membershipID int64
}

func addTeamOwner(t *testing.T, p tokenPanel, email string) teamOwner {
	t.Helper()
	c, csrf, userID := p.member(t, email, "owner")
	var membershipID int64
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`INSERT INTO control_panel_memberships (control_panel_user_id, tenant_id, role)
		 VALUES ($1, $2, 'owner') RETURNING id`, userID, p.tenantID).Scan(&membershipID))
	return teamOwner{client: c, csrf: csrf, membershipID: membershipID}
}

// secondPanelServer mounts the control panel again on the same database with
// its own authorizer, like a second app server. Its Casbin policy is loaded
// now and reloads only every 10 s.
func secondPanelServer(t *testing.T, p tokenPanel) *httptest.Server {
	t.Helper()
	pool := db.NewPool(p.raw)
	authz, err := rbac.NewAuthorizer(pool)
	require.NoError(t, err)
	t.Cleanup(authz.Close)
	noopMailer, err := mailer.New("noop", "", "", "", "noreply@test", "off")
	require.NoError(t, err)
	credCipher, err := secretseal.Load(context.Background(), pool, "")
	require.NoError(t, err)
	root := chi.NewRouter()
	root.Mount(pathControlPanel, controlpanel.New(controlpanel.Deps{
		Pool: pool, Mailer: noopMailer, RBAC: authz, CredentialCipher: credCipher,
		VerifySigningKey: []byte(testEmailVerifySigningKey),
		Config:           controlpanel.Config{Mount: true},
	}))
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)
	return srv
}

func removeTeamMember(t *testing.T, srvURL string, tenantID int64, actor teamOwner, membershipID int64) {
	t.Helper()
	target := fmt.Sprintf("%s%s/tenants/%d/team/members/%d/remove", srvURL, pathControlPanel, tenantID, membershipID)
	resp, err := actor.client.PostForm(target, url.Values{"_csrf": {actor.csrf}})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
}

func TestTeamRemove_should_keep_one_owner_when_a_removed_owner_acts_on_another_server(t *testing.T) {
	p := newTokenPanel(t, false)
	a := addTeamOwner(t, p, "owner-a@example.com")
	b := addTeamOwner(t, p, "owner-b@example.com")
	other := secondPanelServer(t, p)
	removeTeamMember(t, p.srv.URL, p.tenantID, a, b.membershipID)

	removeTeamMember(t, other.URL, p.tenantID, b, a.membershipID)

	var owners int
	require.NoError(t, p.raw.QueryRow(context.Background(),
		`SELECT count(*) FROM control_panel_memberships WHERE tenant_id = $1 AND role = 'owner'`,
		p.tenantID).Scan(&owners))
	assert.Equal(t, 1, owners)
}

// removeWhileLocked holds lockClause on the tenant row, starts A's removal of
// B, and reports whether the removal finished while the lock was held.
func removeWhileLocked(t *testing.T, lockClause string) bool {
	t.Helper()
	p := newTokenPanel(t, false)
	a := addTeamOwner(t, p, "owner-a@example.com")
	b := addTeamOwner(t, p, "owner-b@example.com")
	ctx := context.Background()
	holder, err := p.raw.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	_, err = holder.Exec(ctx, `SELECT 1 FROM tenants WHERE id = $1 `+lockClause, p.tenantID)
	require.NoError(t, err)

	target := fmt.Sprintf("%s%s/tenants/%d/team/members/%d/remove", p.srv.URL, pathControlPanel, p.tenantID, b.membershipID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, perr := a.client.PostForm(target, url.Values{"_csrf": {a.csrf}})
		if perr == nil {
			resp.Body.Close()
		}
	}()
	finished := false
	select {
	case <-done:
		finished = true
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, holder.Rollback(ctx))
	<-done
	return finished
}

func TestTeamRemove_should_wait_for_the_tenant_row_lock(t *testing.T) {
	finished := removeWhileLocked(t, "FOR NO KEY UPDATE")

	assert.False(t, finished, "removal did not wait for the tenant row lock")
}

func TestTeamRemove_should_not_wait_for_a_foreign_key_share_lock(t *testing.T) {
	finished := removeWhileLocked(t, "FOR KEY SHARE")

	assert.True(t, finished, "removal waited for a foreign-key share lock")
}
