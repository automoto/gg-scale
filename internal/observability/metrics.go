package observability

import "github.com/prometheus/client_golang/prometheus"

// Metrics holds the business/health counters that sit above the transport-level
// HTTP and DB instrumentation. One instance is built in main from the process
// registry and threaded into the subsystems that emit events.
//
// Every method is nil-safe: a nil *Metrics is a no-op, so handlers constructed
// without metrics (unit tests) run unchanged. Label sets are deliberately
// low-cardinality — no tenant/project/user IDs — so the series count stays
// bounded no matter how many tenants exist.
type Metrics struct {
	signups                  *prometheus.CounterVec
	verifications            *prometheus.CounterVec
	logins                   *prometheus.CounterVec
	invitesSent              *prometheus.CounterVec
	friendRequests           *prometheus.CounterVec
	bansIssued               *prometheus.CounterVec
	playerSessions           *prometheus.CounterVec
	matchmakerTicket         prometheus.Counter
	matchmakerMatch          prometheus.Counter
	matchmakerShortCommit    prometheus.Counter
	matchmakerQueryReject    prometheus.Counter
	matchmakerTicketFailures *prometheus.CounterVec
	matchmakerTimeToMatch    prometheus.Histogram
	matchmakerQueueDepth     *prometheus.GaugeVec
	matchmakerOldestTicket   *prometheus.GaugeVec
	relayCreds               prometheus.Counter
	mailSends                *prometheus.CounterVec
	quotaRejections          *prometheus.CounterVec
	entitlementApplies       *prometheus.CounterVec
}

// Signup kinds.
const (
	SignupAccount          = "account"            // global player account
	SignupPlayer           = "player"             // per-project player identity
	SignupControlPanelUser = "control_panel_user" // control panel operator
)

// Verification results.
const (
	VerifyOK        = "ok"
	VerifyInvalid   = "invalid"
	VerifyExpired   = "expired"
	VerifyThrottled = "throttled"
)

// Login surfaces and results.
const (
	SurfaceAPI          = "api"
	SurfaceControlPanel = "control_panel"
	SurfacePlayer       = "player"

	LoginOK         = "ok"
	LoginInvalid    = "invalid"
	LoginLocked     = "locked"
	LoginUnverified = "unverified"
	// LoginTwoFactorRequired counts password-valid logins parked at the
	// TOTP challenge; the eventual outcome lands as ok/invalid/locked.
	LoginTwoFactorRequired = "2fa_required"
)

// Invite kinds.
const (
	InviteTeam   = "team"
	InvitePlayer = "player"
)

// Friend-request actions.
const (
	FriendRequestSent      = "sent"
	FriendRequestAccepted  = "accepted"
	FriendRequestDeclined  = "declined"
	FriendRequestCancelled = "cancelled"
	FriendRemoved          = "removed"
)

// Ban scopes.
const (
	BanScopeTenant  = "tenant"
	BanScopeProject = "project"
)

// Player-session lifecycle events.
const (
	SessionOpened = "opened"
	SessionClosed = "closed"
)

// Mail send results.
const (
	MailOK    = "ok"
	MailError = "error"
)

// Entitlement API apply outcomes.
const (
	EntitlementChanged  = "changed"
	EntitlementNoOp     = "noop"
	EntitlementRejected = "rejected"
)

// Matchmaker ticket-failure reasons (the `reason` label on
// ggscale_matchmaker_ticket_failures_total).
const (
	MatchmakerFailureExpired           = "expired"
	MatchmakerFailureAttemptsExhausted = "attempts_exhausted"
)

// MatchmakerBucketSample is one sampled matchmaker bucket, fed to
// SetMatchmakerQueueStats to drive the queue-depth and oldest-ticket-age
// gauges. Region is blank for non-fleet modes. There is no game_mode dimension:
// it is developer-supplied free text and would make the series count unbounded.
type MatchmakerBucketSample struct {
	Mode             string
	Region           string
	Depth            float64
	OldestAgeSeconds float64
}

// NewMetrics registers the business metrics on reg. Call once per process; it
// uses MustRegister because the process owns a single registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		signups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_signups_total",
			Help: "Successful signups by identity kind.",
		}, []string{"kind"}),
		verifications: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_verification_attempts_total",
			Help: "Email-verification attempts by result.",
		}, []string{"result"}),
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_logins_total",
			Help: "Login attempts by surface and result.",
		}, []string{"surface", "result"}),
		invitesSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_invites_sent_total",
			Help: "Invites successfully created by kind.",
		}, []string{"kind"}),
		friendRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_friend_requests_total",
			Help: "Friend-graph actions by type.",
		}, []string{"action"}),
		bansIssued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_bans_issued_total",
			Help: "Player bans issued by scope.",
		}, []string{"scope"}),
		playerSessions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_player_sessions_total",
			Help: "Player-site session lifecycle events (opened at login, closed at logout). " +
				"Expiry/crash closes are not counted here — a precise concurrent gauge is deferred " +
				"(cross-tenant count needs an RLS-bypassing aggregate).",
		}, []string{"event"}),
		matchmakerTicket: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ggscale_matchmaker_tickets_total",
			Help: "Matchmaker tickets enqueued.",
		}),
		matchmakerMatch: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ggscale_matchmaker_matches_total",
			Help: "Matchmaker matches formed.",
		}),
		matchmakerShortCommit: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ggscale_matchmaker_short_commits_total",
			Help: "Groups rolled back because a member drifted between claim and commit; survivors were returned to the queue.",
		}),
		matchmakerQueryReject: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ggscale_matchmaker_query_rejections_total",
			Help: "Candidate pairings rejected by mutual query acceptance; high values point at overly strict ticket queries.",
		}),
		matchmakerTicketFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_matchmaker_ticket_failures_total",
			Help: "Matchmaker tickets that ended in 'failed', by reason (expired / attempts_exhausted).",
		}, []string{"reason"}),
		matchmakerTimeToMatch: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "ggscale_matchmaker_time_to_match_seconds",
			Help: "Queued→matched latency per committed ticket. Buckets span ~0.5s→~34m to bracket the 10m default ticket TTL.",
			// ExponentialBuckets(0.5, 2, 12): 0.5s, 1s, 2s ... ~1024s.
			Buckets: prometheus.ExponentialBuckets(0.5, 2, 12),
		}),
		matchmakerQueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ggscale_matchmaker_queue_depth",
			Help: "Queued, unclaimed matchmaker tickets per bucket, sampled on the sweep cadence.",
		}, []string{"mode", "region"}),
		matchmakerOldestTicket: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ggscale_matchmaker_oldest_ticket_age_seconds",
			Help: "Age of the oldest queued ticket per bucket (head-of-line-blocking early warning), sampled on the sweep cadence.",
		}, []string{"mode", "region"}),
		relayCreds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ggscale_relay_credentials_issued_total",
			Help: "Relay (TURN) credential sets issued.",
		}),
		mailSends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_mail_sends_total",
			Help: "Transactional mail sends by result.",
		}, []string{"result"}),
		quotaRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_quota_rejections_total",
			Help: "New-growth operations rejected by an enforced tenant quota, by axis (projects/players/storage).",
		}, []string{"axis"}),
		entitlementApplies: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_entitlement_apply_total",
			Help: "Entitlement API applies by outcome (changed/noop/rejected).",
		}, []string{"outcome"}),
	}
	reg.MustRegister(
		m.signups, m.verifications, m.logins, m.invitesSent, m.friendRequests,
		m.bansIssued, m.playerSessions, m.matchmakerTicket, m.matchmakerMatch,
		m.matchmakerShortCommit, m.matchmakerQueryReject, m.matchmakerTicketFailures,
		m.matchmakerTimeToMatch, m.matchmakerQueueDepth, m.matchmakerOldestTicket,
		m.relayCreds, m.mailSends, m.quotaRejections, m.entitlementApplies,
	)
	return m
}

// Signup counts a successful signup of the given kind.
func (m *Metrics) Signup(kind string) {
	if m == nil {
		return
	}
	m.signups.WithLabelValues(kind).Inc()
}

// Verification counts a verification attempt with the given result.
func (m *Metrics) Verification(result string) {
	if m == nil {
		return
	}
	m.verifications.WithLabelValues(result).Inc()
}

// Login counts a login attempt on a surface with a result.
func (m *Metrics) Login(surface, result string) {
	if m == nil {
		return
	}
	m.logins.WithLabelValues(surface, result).Inc()
}

// InviteSent counts a created invite of the given kind.
func (m *Metrics) InviteSent(kind string) {
	if m == nil {
		return
	}
	m.invitesSent.WithLabelValues(kind).Inc()
}

// FriendRequest counts a friend-graph action.
func (m *Metrics) FriendRequest(action string) {
	if m == nil {
		return
	}
	m.friendRequests.WithLabelValues(action).Inc()
}

// BanIssued counts a ban issued at the given scope.
func (m *Metrics) BanIssued(scope string) {
	if m == nil {
		return
	}
	m.bansIssued.WithLabelValues(scope).Inc()
}

// PlayerSessionOpened counts a session opened at login.
func (m *Metrics) PlayerSessionOpened() {
	if m == nil {
		return
	}
	m.playerSessions.WithLabelValues(SessionOpened).Inc()
}

// PlayerSessionClosed counts a session closed at logout.
func (m *Metrics) PlayerSessionClosed() {
	if m == nil {
		return
	}
	m.playerSessions.WithLabelValues(SessionClosed).Inc()
}

// MatchmakerTicket counts an enqueued matchmaker ticket.
func (m *Metrics) MatchmakerTicket() {
	if m == nil {
		return
	}
	m.matchmakerTicket.Inc()
}

// MatchmakerMatch counts a formed match.
func (m *Metrics) MatchmakerMatch() {
	if m == nil {
		return
	}
	m.matchmakerMatch.Inc()
}

// MatchmakerShortCommit counts a group rolled back because a member drifted
// between claim and commit.
func (m *Metrics) MatchmakerShortCommit() {
	if m == nil {
		return
	}
	m.matchmakerShortCommit.Inc()
}

// MatchmakerQueryReject counts a candidate pairing rejected by mutual
// query acceptance.
func (m *Metrics) MatchmakerQueryReject() {
	if m == nil {
		return
	}
	m.matchmakerQueryReject.Inc()
}

// MatchmakerTicketFailure counts n tickets flipped to 'failed' for the given
// reason (MatchmakerFailureExpired / MatchmakerFailureAttemptsExhausted).
func (m *Metrics) MatchmakerTicketFailure(reason string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.matchmakerTicketFailures.WithLabelValues(reason).Add(float64(n))
}

// MatchmakerTimeToMatch observes one ticket's queued→matched latency in
// seconds.
func (m *Metrics) MatchmakerTimeToMatch(seconds float64) {
	if m == nil {
		return
	}
	m.matchmakerTimeToMatch.Observe(seconds)
}

// SetMatchmakerQueueStats replaces the queue-depth and oldest-ticket-age
// gauge series with a fresh sample. Both vecs are reset first so buckets that
// drained since the last pass read 0 rather than a stale last value.
func (m *Metrics) SetMatchmakerQueueStats(samples []MatchmakerBucketSample) {
	if m == nil {
		return
	}
	m.matchmakerQueueDepth.Reset()
	m.matchmakerOldestTicket.Reset()
	for _, s := range samples {
		m.matchmakerQueueDepth.WithLabelValues(s.Mode, s.Region).Set(s.Depth)
		m.matchmakerOldestTicket.WithLabelValues(s.Mode, s.Region).Set(s.OldestAgeSeconds)
	}
}

// RelayCredentialIssued counts one issued relay credential set.
func (m *Metrics) RelayCredentialIssued() {
	if m == nil {
		return
	}
	m.relayCreds.Inc()
}

// MailSend counts a mail send by result (see MailOK / MailError).
func (m *Metrics) MailSend(result string) {
	if m == nil {
		return
	}
	m.mailSends.WithLabelValues(result).Inc()
}

// QuotaRejection counts a new-growth operation rejected by an enforced tenant
// quota, labelled by axis (quota.AxisProjects / AxisPlayers / AxisStorage).
func (m *Metrics) QuotaRejection(axis string) {
	if m == nil {
		return
	}
	m.quotaRejections.WithLabelValues(axis).Inc()
}

// EntitlementApply counts an entitlement API apply by outcome (see
// EntitlementChanged / EntitlementNoOp / EntitlementRejected).
func (m *Metrics) EntitlementApply(outcome string) {
	if m == nil {
		return
	}
	m.entitlementApplies.WithLabelValues(outcome).Inc()
}
