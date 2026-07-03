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
	signups          *prometheus.CounterVec
	verifications    *prometheus.CounterVec
	logins           *prometheus.CounterVec
	invitesSent      *prometheus.CounterVec
	friendRequests   *prometheus.CounterVec
	bansIssued       *prometheus.CounterVec
	playerSessions   *prometheus.CounterVec
	matchmakerTicket prometheus.Counter
	matchmakerMatch  prometheus.Counter
	relayCreds       prometheus.Counter
	mailSends        *prometheus.CounterVec
}

// Signup kinds.
const (
	SignupAccount       = "account"        // global player account
	SignupPlayer        = "player"         // per-project player identity
	SignupDashboardUser = "dashboard_user" // dashboard operator
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
	SurfaceAPI       = "api"
	SurfaceDashboard = "dashboard"
	SurfacePlayer    = "player"

	LoginOK         = "ok"
	LoginInvalid    = "invalid"
	LoginLocked     = "locked"
	LoginUnverified = "unverified"
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
		relayCreds: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ggscale_relay_credentials_issued_total",
			Help: "Relay (TURN) credential sets issued.",
		}),
		mailSends: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ggscale_mail_sends_total",
			Help: "Transactional mail sends by result.",
		}, []string{"result"}),
	}
	reg.MustRegister(
		m.signups, m.verifications, m.logins, m.invitesSent, m.friendRequests,
		m.bansIssued, m.playerSessions, m.matchmakerTicket, m.matchmakerMatch,
		m.relayCreds, m.mailSends,
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
