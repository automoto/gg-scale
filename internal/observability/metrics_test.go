package observability_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/observability"
)

func TestNewMetrics_counts_with_labels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.Signup(observability.SignupAccount)
	m.Signup(observability.SignupAccount)
	m.Login(observability.SurfaceControlPanel, observability.LoginOK)

	expected := `
# HELP ggscale_signups_total Successful signups by identity kind.
# TYPE ggscale_signups_total counter
ggscale_signups_total{kind="account"} 2
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_signups_total"))
	assert.Equal(t, 1, testutil.CollectAndCount(reg, "ggscale_logins_total"))
}

func TestMetrics_player_session_lifecycle_counts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.PlayerSessionOpened()
	m.PlayerSessionOpened()
	m.PlayerSessionClosed()

	expected := `
# HELP ggscale_player_sessions_total Player-site session lifecycle events (opened at login, closed at logout). Expiry/crash closes are not counted here — a precise concurrent gauge is deferred (cross-tenant count needs an RLS-bypassing aggregate).
# TYPE ggscale_player_sessions_total counter
ggscale_player_sessions_total{event="opened"} 2
ggscale_player_sessions_total{event="closed"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_player_sessions_total"))
}

func TestMetrics_nil_is_noop(t *testing.T) {
	var m *observability.Metrics
	assert.NotPanics(t, func() {
		m.Signup(observability.SignupPlayer)
		m.Verification(observability.VerifyOK)
		m.Login(observability.SurfaceAPI, observability.LoginInvalid)
		m.InviteSent(observability.InviteTeam)
		m.FriendRequest(observability.FriendRequestSent)
		m.BanIssued(observability.BanScopeTenant)
		m.PlayerSessionOpened()
		m.PlayerSessionClosed()
		m.MatchmakerTicket()
		m.MatchmakerMatch()
		m.RelayCredentialIssued()
		m.MailSend(observability.MailOK)
	})
}
