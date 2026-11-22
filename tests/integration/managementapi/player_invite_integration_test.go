//go:build integration

package managementapi_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/ratelimit"
)

const (
	invitePath        = "/v1/project/players/invite"
	inviteTargetEmail = "newbie@example.com"
	inviteBody        = `{"email":"` + inviteTargetEmail + `"}`
)

func linkPath(id int64) string { return playerPath(id) + "/link" }

// newInviteFixture serves the router with a test mailer, so no real email
// leaves the test.
func newInviteFixture(t *testing.T) (*fixture, *mailer.Recorder) {
	t.Helper()
	rec := &mailer.Recorder{}
	f := newFixtureWith(t, func(d *httpapi.Deps) {
		d.Mailer = rec
		d.MailFrom = "no-reply@example.test"
		d.ControlPanel.BaseURL = "http://app.example.test"
		d.EmailVerifySigningKey = []byte("0123456789abcdef0123456789abcdef")
		d.RateLimitOverrides = ratelimit.NewDBOverrideStore(d.Pool)
	})
	return f, rec
}

func (f *fixture) inviteAudit(action string, tokenID int64) []string {
	f.t.Helper()
	rows, err := f.owner.Query(context.Background(),
		`SELECT payload::text FROM platform_audit_log WHERE action = $1 AND actor_service = $2`,
		action, "management_token:"+strconv.FormatInt(tokenID, 10))
	require.NoError(f.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		require.NoError(f.t, rows.Scan(&p))
		out = append(out, p)
	}
	return out
}

func TestPlayerInvite_should_return_403_without_the_invite_scope(t *testing.T) {
	f, rec := newInviteFixture(t)
	id := f.player(f.projectA)
	tok := f.scopedToken(managementtoken.ScopePlayersRead, managementtoken.ScopePlayersWrite)

	invite := f.do(tok, http.MethodPost, invitePath, inviteBody)
	link := f.do(tok, http.MethodPost, linkPath(id), inviteBody)

	assert.Equal(t, []any{403, 403, 0, 0}, []any{invite.status, link.status, len(rec.Snapshot()),
		f.count(`SELECT count(*) FROM player_invitations`)})
}

func TestPlayerInvite_should_return_403_when_the_creator_lacks_the_players_pair(t *testing.T) {
	f, rec := newInviteFixture(t)
	id := f.player(f.projectA)
	admin := f.user("admin@example.com", f.tenantA, "admin")
	tok, _ := f.token(f.tenantA, f.projectA, admin, []string{managementtoken.ScopePlayersInvite}, time.Hour)
	f.exec(`DELETE FROM casbin_rule WHERE ptype = 'p' AND v0 = 'role:tenant_admin' AND v2 = 'project:*:players' AND v3 = 'manage'`)
	require.NoError(t, f.authz.ReloadPolicy())

	invite := f.do(tok, http.MethodPost, invitePath, inviteBody)
	link := f.do(tok, http.MethodPost, linkPath(id), inviteBody)

	assert.Equal(t, []any{403, 403, 0, 0}, []any{invite.status, link.status, len(rec.Snapshot()),
		f.count(`SELECT count(*) FROM player_invitations`)}, invite.raw+link.raw)
}

func TestPlayerInvite_should_send_one_email_and_audit_the_token_without_the_address(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopePlayersInvite}, time.Hour)

	got := f.do(tok, http.MethodPost, invitePath, inviteBody)

	sent := rec.Snapshot()
	require.Len(t, sent, 1, got.raw)
	audit := f.inviteAudit("control_panel.player.invite", tokenID)
	require.Len(t, audit, 1)
	_, hasEmail := got.body["email"]
	assert.Equal(t, []any{200, false, []string{inviteTargetEmail}, 1, false},
		[]any{got.status, hasEmail, sent[0].To, f.count(`SELECT count(*) FROM player_invitations WHERE email = $1`, inviteTargetEmail),
			strings.Contains(audit[0]+got.raw, inviteTargetEmail)}, got.raw)
	assert.Contains(t, sent[0].Body, "/v1/players/p/"+strconv.FormatInt(f.projectA, 10)+"/invite/accept?code=")
}

func TestPlayerInvite_should_return_the_address_only_with_players_pii(t *testing.T) {
	f, _ := newInviteFixture(t)
	tok := f.scopedToken(managementtoken.ScopePlayersInvite, managementtoken.ScopePlayersRead, managementtoken.ScopePlayersPII)

	got := f.do(tok, http.MethodPost, invitePath, inviteBody)

	assert.Equal(t, []any{200, inviteTargetEmail}, []any{got.status, got.body["email"]}, got.raw)
}

func TestPlayerInvite_should_return_422_for_an_invalid_address(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok := f.scopedToken(managementtoken.ScopePlayersInvite)

	got := f.do(tok, http.MethodPost, invitePath, `{"email":"not-an-email"}`)

	assert.Equal(t, []any{422, 0}, []any{got.status, len(rec.Snapshot())}, got.raw)
}

func TestPlayerInvite_should_return_429_with_retry_after_when_the_project_limit_is_used(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok := f.scopedToken(managementtoken.ScopePlayersInvite, managementtoken.ScopeLimitsWrite)
	require.Equal(t, 200, f.do(tok, http.MethodPut, inviteLimitsPath, `{"inviter_per_hour":1,"domain_per_day":0}`).status)

	first := f.do(tok, http.MethodPost, invitePath, inviteBody)
	second := f.do(tok, http.MethodPost, invitePath, `{"email":"other@example.org"}`)

	retry, _ := strconv.Atoi(second.header.Get("Retry-After"))
	assert.Equal(t, []any{200, 429, true, 1, 1}, []any{first.status, second.status, retry >= 1, len(rec.Snapshot()),
		f.count(`SELECT count(*) FROM player_invitations`)}, second.raw)
}

func TestPlayerLink_should_send_a_link_invite_and_audit_the_token(t *testing.T) {
	f, rec := newInviteFixture(t)
	id := f.player(f.projectA)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, []string{managementtoken.ScopePlayersInvite}, time.Hour)

	got := f.do(tok, http.MethodPost, linkPath(id), inviteBody)

	audit := f.inviteAudit("control_panel.player.link", tokenID)
	var target *int64
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT project_player_id FROM player_invitations WHERE email = $1`, inviteTargetEmail).Scan(&target))
	require.NotNil(t, target)
	assert.Equal(t, []any{200, id, 1, 1, float64(id)},
		[]any{got.status, *target, len(rec.Snapshot()), len(audit), got.body["player_id"]}, got.raw)
}

func TestPlayerLink_should_return_404_for_a_player_of_another_project_or_tenant(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok := f.scopedToken(managementtoken.ScopePlayersInvite)
	var statuses []int
	for _, id := range []int64{f.player(f.projectB), f.player(f.projectC), 999999} {
		statuses = append(statuses, f.do(tok, http.MethodPost, linkPath(id), inviteBody).status)
	}

	assert.Equal(t, []any{[]int{404, 404, 404}, 0, 0}, []any{statuses, len(rec.Snapshot()),
		f.count(`SELECT count(*) FROM player_invitations`)})
}

func TestPlayerLink_should_return_409_when_another_player_has_the_address(t *testing.T) {
	f, rec := newInviteFixture(t)
	f.namedPlayer(f.projectA)
	id := f.player(f.projectA)
	tok := f.scopedToken(managementtoken.ScopePlayersInvite)

	got := f.do(tok, http.MethodPost, linkPath(id), `{"email":"`+playerEmail+`"}`)

	assert.Equal(t, []any{409, 0}, []any{got.status, len(rec.Snapshot())}, got.raw)
}

func TestPlayerInvite_tools_should_send_and_link(t *testing.T) {
	f, rec := newInviteFixture(t)
	id := f.player(f.projectA)
	tok := f.scopedToken(managementtoken.ScopePlayersInvite)

	invited := structured(f.call(tok, "invite_player", map[string]any{"email": inviteTargetEmail}))
	linked := structured(f.call(tok, "link_player", map[string]any{"player_id": id, "email": "linked@example.com"}))

	assert.Equal(t, []any{true, float64(id), 2}, []any{invited["invite_id"] != nil, linked["player_id"], len(rec.Snapshot())})
}
