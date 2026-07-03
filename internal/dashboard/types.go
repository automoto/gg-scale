// Package dashboard implements the M1 server-rendered admin dashboard.
package dashboard

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Config controls dashboard mounting and cookie behavior.
type Config struct {
	Mount        bool
	CookieSecure bool
	// BaseURL is the externally-visible origin (scheme + host) prepended
	// to magic links emitted in invite emails. Empty means "use a
	// relative path" — fine for dev but not for production.
	BaseURL string
	// MailFrom is the From: address used by invitation and verification
	// emails. Empty disables emails (a recorder/log-only fallback can
	// still surface the codes through audit logs).
	MailFrom string
	// TrustedProxyHeader is honored for audit IPs only when RemoteAddr is in
	// TrustedProxyCIDRs. Empty disables forwarded-IP trust.
	TrustedProxyHeader string
	TrustedProxyCIDRs  []string
	// FleetEnabled mirrors the FEATURE_FLEET_ENABLED startup switch. When
	// false the dashboard hides every dedicated-server fleet surface and its
	// routes 404, so operators can't configure a feature the process refuses
	// to run.
	FleetEnabled bool
	// RelayEnabled mirrors the FEATURE_P2P_RELAY_ENABLED startup switch. Gates
	// whether the p2p_relay per-key scope can be granted from the dashboard.
	RelayEnabled bool
}

// Enabled reports whether the dashboard should be mounted.
func (c Config) Enabled() bool {
	return c.Mount
}

// LoginView is the data rendered by the dashboard login template.
type LoginView struct {
	Email       string
	Error       string
	FieldErrors map[string]string
}

// SetupTokenView is the data rendered by step 1 of first-run setup.
type SetupTokenView struct {
	TokenFilePath string
	Error         string
	FieldErrors   map[string]string
}

// SetupAdminView is the data rendered by step 2 of first-run setup.
type SetupAdminView struct {
	Token       string
	Email       string
	Error       string
	FieldErrors map[string]string
}

// TenantView is one tenant visible to a dashboard user.
type TenantView struct {
	ID        int64
	Name      string
	Role      string
	CreatedAt time.Time
}

// HomeView is the data rendered by the dashboard landing page.
type HomeView struct {
	UserEmail       string
	CSRFToken       string
	Tenants         []TenantView
	IsPlatformAdmin bool
}

// SignupSuccessView shows a one-time plaintext API key after creation.
type SignupSuccessView struct {
	TenantID  int64
	ProjectID int64
	APIKeyID  int64
	APIKey    string
}

// APIKeyView is one API key row in the dashboard key table.
type APIKeyView struct {
	ID          int64
	ProjectID   *int64
	ProjectName string
	Label       string
	CreatedAt   time.Time
	RevokedAt   *time.Time
	// Scopes are the per-key feature grants currently set (e.g. "fleet",
	// "p2p_relay").
	Scopes []string
	// FleetGrantable / RelayGrantable report whether the matching feature is
	// enabled (env kill switch on AND a feature_grant row exists) for this
	// key's tenant/project, so the UI can offer a grant toggle instead of
	// "no access".
	FleetGrantable bool
	RelayGrantable bool
}

// HasScope reports whether the key currently holds scope.
func (v APIKeyView) HasScope(scope string) bool {
	for _, s := range v.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// ProjectOption is one project pickable in the API-key creation form.
type ProjectOption struct {
	ID                   int64
	Name                 string
	CreatedAt            time.Time
	PublicJoiningEnabled bool
}

// NewTenantView is the data rendered by the create-tenant page.
type NewTenantView struct {
	UserEmail   string
	CSRFToken   string
	Error       string
	FieldErrors map[string]string
}

// ProjectsView is the data rendered by the per-tenant projects page.
type ProjectsView struct {
	UserEmail string
	TenantID  int64
	CSRFToken string
	Projects  []ProjectOption
	Message   string
	// TenantPublicJoining is the tenant master switch. Effective per-project
	// join = TenantPublicJoining AND project.PublicJoiningEnabled.
	TenantPublicJoining bool
	// FleetEnabled hides the per-project Fleets/Allocations actions when the
	// FEATURE_FLEET_ENABLED kill switch is off.
	FleetEnabled bool
}

// NewProjectView is the data rendered by the create-project page.
type NewProjectView struct {
	UserEmail   string
	CSRFToken   string
	TenantID    int64
	Name        string
	Error       string
	FieldErrors map[string]string
}

// NewAPIKeyView is the data rendered by the create-api-key page.
type NewAPIKeyView struct {
	UserEmail string
	CSRFToken string
	TenantID  int64
	Projects  []ProjectOption
	Label     string
	ProjectID string
	// KeyType is the currently-selected type — either "publishable"
	// (embedded in shipped game clients) or "secret" (game-server /
	// backend only). Empty during initial render to surface the default
	// selection in the form.
	KeyType     string
	Error       string
	FieldErrors map[string]string
}

// APIKeysView is the data rendered by the API-key management page.
type APIKeysView struct {
	UserEmail string
	TenantID  int64
	CSRFToken string
	Keys      []APIKeyView
	Message   string
}

// HelpView is the data rendered by the in-app concepts page.
type HelpView struct {
	UserEmail string
	CSRFToken string
}

// AccountView is the data rendered by the dashboard account page.
type AccountView struct {
	UserEmail   string
	CSRFToken   string
	Message     string
	Error       string
	FieldErrors map[string]string
}

// TeamMemberView is one row of the tenant team table.
type TeamMemberView struct {
	MembershipID    int64
	UserID          int64
	Email           string
	Role            string
	IsPlatformAdmin bool
	LastLoginAt     time.Time
	JoinedAt        time.Time
	// FleetOperator reports whether the member holds the à-la-carte
	// role:fleet_operator grant in this tenant (in addition to their
	// membership role).
	FleetOperator bool
}

// PendingInviteView is one row of the pending-invitations table.
type PendingInviteView struct {
	ID          int64
	Email       string
	Role        string
	InvitedByID int64
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// TeamView is the data rendered by the tenant team page.
type TeamView struct {
	UserEmail string
	CSRFToken string
	TenantID  int64
	IsOwnerID int64
	Members   []TeamMemberView
	Pending   []PendingInviteView
	Message   string
	Error     string
	// FleetEnabled mirrors the FEATURE_FLEET_ENABLED switch. The fleet-operator
	// grant control only appears when fleets are enabled — granting it while
	// the feature is off would be a no-op (all fleet surfaces 404).
	FleetEnabled bool
}

// InviteTeamView is the data rendered by the invite-teammate form.
type InviteTeamView struct {
	UserEmail   string
	CSRFToken   string
	TenantID    int64
	Email       string
	Role        string
	Error       string
	FieldErrors map[string]string
}

// PlatformTeamView is the data rendered by the platform admin team page.
type PlatformTeamView struct {
	UserEmail string
	CSRFToken string
	Admins    []TeamMemberView
	Pending   []PendingInviteView
	Message   string
	Error     string
}

// InvitePlatformAdminView is the form rendered to invite another platform admin.
type InvitePlatformAdminView struct {
	UserEmail   string
	CSRFToken   string
	Email       string
	Error       string
	FieldErrors map[string]string
}

// InvitePlayerView is the data rendered by the player invite form.
type InvitePlayerView struct {
	UserEmail   string
	CSRFToken   string
	TenantID    int64
	ProjectID   int64
	Email       string
	Error       string
	FieldErrors map[string]string
}

// UserView is one row of the /admin/users table.
type UserView struct {
	ID              int64
	Email           string
	IsPlatformAdmin bool
	DisabledAt      time.Time
	LastLoginAt     time.Time
	CreatedAt       time.Time
	TenantCount     int64
	IsSelf          bool // hides the disable button for the current actor
}

// PlatformUsersView is the data rendered by /v1/dashboard/admin/users.
type PlatformUsersView struct {
	UserEmail string
	CSRFToken string
	Search    string
	Users     []UserView
	Total     int64
	Page      int
	HasPrev   bool
	HasNext   bool
	Message   string
}

// PlayerAccountView is one row in the platform-admin player-accounts list.
type PlayerAccountView struct {
	ID         string // UUID
	Email      string
	Verified   bool
	Disabled   bool
	HasDisplay bool
	CreatedAt  time.Time
}

// PlayerAccountsView is the data rendered by /v1/dashboard/admin/player-accounts.
type PlayerAccountsView struct {
	UserEmail string
	CSRFToken string
	Search    string
	Accounts  []PlayerAccountView
	Message   string
}

// LinkedProjectView is one project a global account is linked to.
type LinkedProjectView struct {
	TenantID    int64
	ProjectID   int64
	ProjectName string
	ExternalID  string
}

// PlayerAccountDetailView renders a single account with its linked projects.
type PlayerAccountDetailView struct {
	UserEmail   string
	CSRFToken   string
	ID          string
	Email       string
	DisplayName string
	Verified    bool
	Disabled    bool
	CreatedAt   time.Time
	Projects    []LinkedProjectView
	Message     string
}

// AcceptInviteView is the data rendered by the invite acceptance page.
type AcceptInviteView struct {
	Code        string
	Email       string
	Role        string
	TenantName  string
	IsPlatform  bool
	NewUser     bool // true means we need to collect a password
	Error       string
	FieldErrors map[string]string
	ExpiresAt   time.Time
	AcceptedAt  time.Time
	CSRFToken   string
}

// AllocationView is one row in the fleet list and the snapshot on the
// allocation-detail page.
type AllocationView struct {
	ID         int64
	ProjectID  int64
	Backend    string
	BackendRef string
	Region     string
	Address    string
	Status     string
}

// EventView is one ring-buffer entry on the fleet detail timeline.
type EventView struct {
	ID         int64
	Status     string
	Address    string
	ErrMessage string
	CreatedAt  time.Time
}

// FleetView is the data rendered by the per-project fleet page.
type FleetView struct {
	UserEmail       string
	CSRFToken       string
	TenantID        int64
	ProjectID       int64
	BackendName     string
	Enabled         bool
	IncludeTerminal bool
	Allocations     []AllocationView
	Total           int64
	Page            int
	HasPrev         bool
	HasNext         bool
	Message         string
}

// FleetDetailView is the data rendered by the per-allocation page.
type FleetDetailView struct {
	UserEmail  string
	CSRFToken  string
	TenantID   int64
	ProjectID  int64
	Allocation AllocationView
	Events     []EventView
	Message    string
}

// NewAllocationView is the data rendered by the manual-allocate form.
type NewAllocationView struct {
	UserEmail   string
	CSRFToken   string
	TenantID    int64
	ProjectID   int64
	BackendName string
	Enabled     bool
	Fleet       string
	Fleets      []FleetOption
	Region      string
	GameMode    string
	Capacity    int
	Error       string
	FieldErrors map[string]string
}

// FleetOption is one entry in the fleet dropdown on the manual-allocate
// form. BackendMatches is false when the template's backend doesn't match
// what ggscale is running; the form disables those options with a hint.
type FleetOption struct {
	ID                int64
	Name              string
	Backend           string
	BackendMatches    bool
	BackendConfigured string
}

// FleetsListView is the data rendered by /fleets.
type FleetsListView struct {
	UserEmail         string
	CSRFToken         string
	TenantID          int64
	ProjectID         int64
	BackendConfigured string
	Enabled           bool
	Fleets            []FleetRowView
	Message           string
}

// FleetRowView is one row on the fleets list.
type FleetRowView struct {
	ID             int64
	Name           string
	Backend        string
	BackendMatches bool
	Summary        string
}

// NewFleetView is the data rendered by /fleets/new and the create form.
type NewFleetView struct {
	UserEmail         string
	CSRFToken         string
	TenantID          int64
	ProjectID         int64
	BackendConfigured string
	Name              string
	Backend           string
	Config            map[string]string
	Error             string
	FieldErrors       map[string]string
}

// EditFleetView is the data rendered by /fleets/{id}.
type EditFleetView struct {
	UserEmail         string
	CSRFToken         string
	TenantID          int64
	ProjectID         int64
	FleetID           int64
	Name              string
	Backend           string
	Config            map[string]string
	BackendConfigured string
	Error             string
	FieldErrors       map[string]string
}

// DeallocateConfirmView is the data rendered by the type-the-ID confirm page.
type DeallocateConfirmView struct {
	UserEmail  string
	CSRFToken  string
	TenantID   int64
	ProjectID  int64
	Allocation AllocationView
	Error      string
}

// BackendRowView is one backend row on the tenant backends page.
type BackendRowView struct {
	Name            string
	AllocationCount int64
}

// FleetBackendsView is the data rendered by the tenant-scoped backends page.
type FleetBackendsView struct {
	UserEmail      string
	CSRFToken      string
	TenantID       int64
	ConfiguredName string
	Enabled        bool
	HealthErr      string
	Backends       []BackendRowView
}

// MatchmakerBucketView is one (region, game_mode, status) bucket row.
type MatchmakerBucketView struct {
	Region   string
	GameMode string
	Status   string
	Count    int64
	Oldest   time.Time
}

// MatchmakerQueueView is the data rendered by the matchmaker queue page.
type MatchmakerQueueView struct {
	UserEmail string
	CSRFToken string
	TenantID  int64
	ProjectID int64
	Buckets   []MatchmakerBucketView
}

// PlatformPluginsView is the data rendered by /admin/plugins.
type PlatformPluginsView struct {
	UserEmail string
	CSRFToken string
	Snapshot  *PluginSnapshot
}

func stringFromInt(n int64) string {
	return strconv.FormatInt(n, 10)
}

func approximateTotalLabel(total int64, hasNext bool) string {
	if hasNext {
		return "at least " + stringFromInt(total)
	}
	return stringFromInt(total)
}

// fleetBackendKind buckets a fleet.backend value ("docker"|"agones"|"plugin:<n>")
// into one of three groups so the create-form switch stays exhaustive without
// duplicating the prefix check at every call site.
func fleetBackendKind(backend string) string {
	switch backend {
	case "docker":
		return "docker"
	case "agones":
		return "agones"
	}
	if strings.HasPrefix(backend, "plugin") {
		return "plugin"
	}
	return "docker"
}

// editToNewFleetView projects an EditFleetView onto the NewFleetView shape
// so EditFleetPage can reuse FleetBackendFieldsFragment. templ struggles
// with multi-line struct literals inside @-calls; the named helper keeps
// the call site one expression.
func editToNewFleetView(vm EditFleetView) NewFleetView {
	return NewFleetView{
		TenantID:    vm.TenantID,
		ProjectID:   vm.ProjectID,
		Backend:     vm.Backend,
		Config:      vm.Config,
		FieldErrors: vm.FieldErrors,
	}
}

// fleetSelectorLabels strips the "selector." prefix from a fleet's config
// map so the agones create form can render one input row per label.
func fleetSelectorLabels(cfg map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfg {
		if rest, ok := strings.CutPrefix(k, "selector."); ok && rest != "" {
			out[rest] = v
		}
	}
	return out
}

// allocationsBasePathTpl is the templ-side equivalent of allocationsBasePath
// in allocations.go; both must agree on the path shape.
func allocationsBasePathTpl(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"
}

// fleetsBasePathTpl is the templ-side equivalent of fleetsBasePath.
func fleetsBasePathTpl(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/fleets"
}

// fleetQuery preserves the include-terminal toggle + current page across
// the polled fragment URL so the timer-driven refresh doesn't reset filters.
func fleetQuery(vm FleetView) string {
	q := "page=" + strconv.Itoa(vm.Page)
	if vm.IncludeTerminal {
		q += "&all=1"
	}
	return q
}

func capacityString(c int) string {
	if c <= 0 {
		return "1"
	}
	return strconv.Itoa(c)
}

// allocationTerminal mirrors fleet.Status.IsTerminal for use in templates,
// which only see the string form of status.
func allocationTerminal(status string) bool {
	return status == "shutdown" || status == "failed"
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func csrfHeaders(token string) string {
	body, err := json.Marshal(map[string]string{csrfHeader: token})
	if err != nil {
		return "{}"
	}
	return string(body)
}
