package observability_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
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

func TestMetrics_matchmaker_ticket_failures_by_reason(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.MatchmakerTicketFailure(observability.MatchmakerFailureExpired, 2)
	m.MatchmakerTicketFailure(observability.MatchmakerFailureAttemptsExhausted, 1)
	m.MatchmakerTicketFailure(observability.MatchmakerFailureExpired, 0)  // no-op
	m.MatchmakerTicketFailure(observability.MatchmakerFailureExpired, -3) // no-op

	expected := `
# HELP ggscale_matchmaker_ticket_failures_total Matchmaker tickets that ended in 'failed', by reason (expired / attempts_exhausted).
# TYPE ggscale_matchmaker_ticket_failures_total counter
ggscale_matchmaker_ticket_failures_total{reason="attempts_exhausted"} 1
ggscale_matchmaker_ticket_failures_total{reason="expired"} 2
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_matchmaker_ticket_failures_total"))
}

func TestMetrics_matchmaker_time_to_match_observes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.MatchmakerTimeToMatch(1.5)
	m.MatchmakerTimeToMatch(3.0)

	// One histogram series with two observations summing to 4.5s.
	mfs, err := reg.Gather()
	require.NoError(t, err)
	var h *dto.Histogram
	for _, mf := range mfs {
		if mf.GetName() == "ggscale_matchmaker_time_to_match_seconds" {
			require.Len(t, mf.GetMetric(), 1)
			h = mf.GetMetric()[0].GetHistogram()
		}
	}
	require.NotNil(t, h)
	assert.Equal(t, uint64(2), h.GetSampleCount())
	assert.InDelta(t, 4.5, h.GetSampleSum(), 0.001)
}

func TestMetrics_matchmaker_queue_stats_reset_between_passes(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := observability.NewMetrics(reg)

	m.SetMatchmakerQueueStats([]observability.MatchmakerBucketSample{
		{Mode: "match_only", Region: "", Depth: 3, OldestAgeSeconds: 42},
		{Mode: "fleet_allocation", Region: "us-east-1", Depth: 1, OldestAgeSeconds: 5},
	})

	expected := `
# HELP ggscale_matchmaker_queue_depth Queued, unclaimed matchmaker tickets per bucket, sampled on the sweep cadence.
# TYPE ggscale_matchmaker_queue_depth gauge
ggscale_matchmaker_queue_depth{mode="match_only",region=""} 3
ggscale_matchmaker_queue_depth{mode="fleet_allocation",region="us-east-1"} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected), "ggscale_matchmaker_queue_depth"))

	// A later pass where the fleet bucket drained: its series must disappear,
	// not linger at the last value.
	m.SetMatchmakerQueueStats([]observability.MatchmakerBucketSample{
		{Mode: "match_only", Region: "", Depth: 1, OldestAgeSeconds: 10},
	})

	expectedAfter := `
# HELP ggscale_matchmaker_queue_depth Queued, unclaimed matchmaker tickets per bucket, sampled on the sweep cadence.
# TYPE ggscale_matchmaker_queue_depth gauge
ggscale_matchmaker_queue_depth{mode="match_only",region=""} 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expectedAfter), "ggscale_matchmaker_queue_depth"))
	assert.Equal(t, 1, testutil.CollectAndCount(reg, "ggscale_matchmaker_oldest_ticket_age_seconds"))
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
		m.MatchmakerTicketFailure(observability.MatchmakerFailureExpired, 1)
		m.MatchmakerTimeToMatch(2.0)
		m.SetMatchmakerQueueStats([]observability.MatchmakerBucketSample{{Mode: "match_only"}})
		m.RelayCredentialIssued()
		m.MailSend(observability.MailOK)
	})
}
