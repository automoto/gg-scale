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

// inviteScopes are the scopes that invite_player and link_player need.
var inviteScopes = []string{managementtoken.ScopePlayersRead, managementtoken.ScopePlayersPII, managementtoken.ScopePlayersInvite}

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
	tok, _ := f.token(f.tenantA, f.projectA, admin, inviteScopes, time.Hour)
	f.dropPair("role:tenant_admin", "project:*:players", "manage")

	invite := f.do(tok, http.MethodPost, invitePath, inviteBody)
	link := f.do(tok, http.MethodPost, linkPath(id), inviteBody)

	assert.Equal(t, []any{403, 403, 0, 0}, []any{invite.status, link.status, len(rec.Snapshot()),
		f.count(`SELECT count(*) FROM player_invitations`)}, invite.raw+link.raw)
}

func TestPlayerInvite_should_send_one_email_and_audit_the_token_without_the_address(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, inviteScopes, time.Hour)

	got := f.do(tok, http.MethodPost, invitePath, inviteBody)

	sent := rec.Snapshot()
	require.Len(t, sent, 1, got.raw)
	service, audited := f.audit("control_panel.player.invite", int64(got.body["invite_id"].(float64)))
	assert.Equal(t, []any{200, []string{inviteTargetEmail}, 1, "management_token:" + strconv.FormatInt(tokenID, 10), 1, 0},
		[]any{got.status, sent[0].To, f.count(`SELECT count(*) FROM player_invitations WHERE email = $1`, inviteTargetEmail),
			service, audited,
			f.count(`SELECT count(*) FROM platform_audit_log WHERE strpos(payload::text, $1) > 0`, inviteTargetEmail)}, got.raw)
	assert.Contains(t, sent[0].Body, "/v1/players/p/"+strconv.FormatInt(f.projectA, 10)+"/invite/accept?code=")
}

func TestPlayerInvite_should_return_the_address(t *testing.T) {
	f, _ := newInviteFixture(t)
	tok := f.scopedToken(inviteScopes...)

	got := f.do(tok, http.MethodPost, invitePath, inviteBody)

	assert.Equal(t, []any{200, inviteTargetEmail}, []any{got.status, got.body["email"]}, got.raw)
}

func TestPlayerInvite_should_return_422_for_an_invalid_address(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok := f.scopedToken(inviteScopes...)

	got := f.do(tok, http.MethodPost, invitePath, `{"email":"not-an-email"}`)

	assert.Equal(t, []any{422, 0}, []any{got.status, len(rec.Snapshot())}, got.raw)
}

func TestPlayerInvite_should_return_429_with_retry_after_when_the_project_limit_is_used(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok := f.scopedToken(append([]string{managementtoken.ScopeLimitsWrite}, inviteScopes...)...)
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
	tok, tokenID := f.token(f.tenantA, f.projectA, f.ownerID, inviteScopes, time.Hour)

	got := f.do(tok, http.MethodPost, linkPath(id), inviteBody)

	service, audited := f.audit("control_panel.player.link", id)
	var target *int64
	require.NoError(t, f.owner.QueryRow(context.Background(),
		`SELECT project_player_id FROM player_invitations WHERE email = $1`, inviteTargetEmail).Scan(&target))
	require.NotNil(t, target)
	assert.Equal(t, []any{200, id, 1, 1, "management_token:" + strconv.FormatInt(tokenID, 10), float64(id)},
		[]any{got.status, *target, len(rec.Snapshot()), audited, service, got.body["player_id"]}, got.raw)
}

func TestPlayerLink_should_return_404_for_a_player_of_another_project_or_tenant(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok := f.scopedToken(inviteScopes...)
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
	tok := f.scopedToken(inviteScopes...)

	got := f.do(tok, http.MethodPost, linkPath(id), `{"email":"`+playerEmail+`"}`)

	assert.Equal(t, []any{409, 0}, []any{got.status, len(rec.Snapshot())}, got.raw)
}

func TestPlayerInvite_tools_should_send_and_link(t *testing.T) {
	f, rec := newInviteFixture(t)
	id := f.player(f.projectA)
	tok := f.scopedToken(inviteScopes...)

	invited := structured(f.call(tok, "invite_player", map[string]any{"email": inviteTargetEmail}))
	linked := structured(f.call(tok, "link_player", map[string]any{"player_id": id, "email": "linked@example.com"}))

	assert.Equal(t, []any{true, float64(id), 2}, []any{invited["invite_id"] != nil, linked["player_id"], len(rec.Snapshot())})
}

func TestPlayerInvite_should_return_422_for_an_ip_literal_domain(t *testing.T) {
	f, rec := newInviteFixture(t)
	tok := f.scopedToken(inviteScopes...)

	got := f.do(tok, http.MethodPost, invitePath, `{"email":"a@[127.0.0.1]"}`)

	assert.Equal(t, []any{422, 0}, []any{got.status, len(rec.Snapshot())}, got.raw)
}

// Without players:pii, the 409 for a taken or pending address would tell
// whether the address belongs to a player. Each address gets the same 403
// before any lookup, send, or invite row.
func TestPlayerInvite_should_return_403_without_players_pii_for_each_address(t *testing.T) {
	f, rec := newInviteFixture(t)
	f.namedPlayer(f.projectA)
	id := f.player(f.projectA)
	require.Equal(t, 200, f.do(f.scopedToken(inviteScopes...), http.MethodPost, invitePath, inviteBody).status)
	tok := f.scopedToken(managementtoken.ScopePlayersRead, managementtoken.ScopePlayersInvite)
	cases := map[string]struct{ path, body string }{
		"invite a pending address": {invitePath, inviteBody},
		"invite a free address":    {invitePath, `{"email":"free@example.com"}`},
		"link a taken address":     {linkPath(id), `{"email":"` + playerEmail + `"}`},
		"link a free address":      {linkPath(id), `{"email":"free@example.com"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := f.do(tok, http.MethodPost, c.path, c.body)

			assert.Equal(t, []any{403, true, false, 1, 1}, []any{got.status, strings.Contains(got.detail(), managementtoken.ScopePlayersPII),
				strings.Contains(got.raw, "already"), len(rec.Snapshot()), f.count(`SELECT count(*) FROM player_invitations`)}, got.raw)
		})
	}
}

func TestPlayerInvite_tools_should_be_refused_without_players_pii(t *testing.T) {
	f, rec := newInviteFixture(t)
	f.namedPlayer(f.projectA)
	id := f.player(f.projectA)
	tok := f.scopedToken(managementtoken.ScopePlayersRead, managementtoken.ScopePlayersInvite)

	invited := f.call(tok, "invite_player", map[string]any{"email": inviteTargetEmail})
	linked := f.call(tok, "link_player", map[string]any{"player_id": id, "email": playerEmail})

	assert.Equal(t, []any{true, true, true, true, 0, 0}, []any{isError(invited), isError(linked),
		strings.Contains(resultDetail(invited), managementtoken.ScopePlayersPII), strings.Contains(resultDetail(linked), managementtoken.ScopePlayersPII),
		len(rec.Snapshot()), f.count(`SELECT count(*) FROM player_invitations`)}, resultText(linked))
}
