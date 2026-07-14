// Package control panel implements the M1 server-rendered admin control panel.
package controlpanel

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Config controls control panel mounting and cookie behavior.
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
	// false the control panel hides every dedicated-server fleet surface and its
	// routes 404, so operators can't configure a feature the process refuses
	// to run.
	FleetEnabled bool
	// RelayEnabled mirrors the FEATURE_P2P_RELAY_ENABLED startup switch. Gates
	// whether the p2p_relay per-key scope can be granted from the control panel.
	RelayEnabled bool
	// StorageMaxValueBytes is the platform default storage value cap, shown on
	// the rate-limits page as the fallback when no tenant/project override is set.
	StorageMaxValueBytes int64
	// ServerSettings is the redacted, read-only snapshot of server-wide (env)
	// configuration shown on the platform-admin server settings page. Built in
	// main.go so raw secrets are reduced to booleans before crossing into this
	// package.
	ServerSettings ServerSettingsSnapshot
}

// ServerSettingsSnapshot is the read-only view of server-wide configuration
// rendered on the platform-admin server settings page. Secrets are represented
// as "configured" booleans only — never their values.
type ServerSettingsSnapshot struct {
	Env      string
	LogLevel string
	HTTPAddr string

	ControlPanelEnabled    bool
	PlayersEnabled         bool
	FeatureFleetEnabled    bool
	FeatureP2PRelayEnabled bool

	FleetBackend string
	FleetRegion  string

	MailProvider    string
	SMTPAddr        string
	SMTPUser        string
	SMTPTLS         string
	MailFrom        string
	SMTPPasswordSet bool

	CORSAllowedOrigins []string

	// Secrets — presence only, never the value.
	JWTConfigured      bool
	RelaySecretSet     bool
	DatabaseConfigured bool
}

// Enabled reports whether the control panel should be mounted.
func (c Config) Enabled() bool {
	return c.Mount
}

// LoginView is the data rendered by the control panel login template.
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

// TenantView is one tenant visible to a control panel user.
type TenantView struct {
	ID        int64
	Name      string
	Role      string
	CreatedAt time.Time
}

type navDestination string

const (
	navTenants         navDestination = "tenants"
	navProjects        navDestination = "projects"
	navAPIKeys         navDestination = "api-keys"
	navTeam            navDestination = "team"
	navTenantSettings  navDestination = "tenant-settings"
	navRateLimits      navDestination = "rate-limits"
	navPlayers         navDestination = "players"
	navLeaderboards    navDestination = "leaderboards"
	navFleets          navDestination = "fleets"
	navAllocations     navDestination = "allocations"
	navMatchmaker      navDestination = "matchmaker"
	navProjectSettings navDestination = "project-settings"
	navPlatformUsers   navDestination = "platform-users"
	navTenantSignups   navDestination = "tenant-signups"
	navPlayerAccounts  navDestination = "player-accounts"
	navPlatformTeam    navDestination = "platform-team"
	navServerSettings  navDestination = "server-settings"
	navPlugins         navDestination = "plugins"
	navHelp            navDestination = "help"
	navAccount         navDestination = "account"
)

// AppNav is the compact, context-aware control-panel navigation model used by
// appLayout. Pages pass only the resource context they already know.
type AppNav struct {
	Active          navDestination
	TenantID        int64
	ProjectID       int64
	IsPlatformAdmin bool
	FleetEnabled    bool
	PluginsEnabled  bool
}

// IsActive reports whether dest is the currently active nav destination.
func (n AppNav) IsActive(dest navDestination) bool {
	return n.Active == dest
}

// HomeView is the data rendered by the control panel landing page.
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

// APIKeyView is one API key row in the control panel key table.
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
	// FleetGrantable / RelayGrantable / MatchmakerGrantable report whether
	// the matching feature is enabled for this key's tenant/project, so the
	// UI can offer a grant toggle instead of "no access". Fleet/relay need
	// their env kill switch on AND a feature_grant row; matchmaker defaults
	// to enabled.
	FleetGrantable      bool
	RelayGrantable      bool
	MatchmakerGrantable bool
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

// HasManagedScope reports whether the key holds any managed feature scope.
func (v APIKeyView) HasManagedScope() bool {
	for _, scope := range managedAPIKeyScopes {
		if v.HasScope(scope) {
			return true
		}
	}
	return false
}

// ProjectOption is one project pickable in the API-key creation form.
type ProjectOption struct {
	ID        int64
	Name      string
	CreatedAt time.Time
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

// RateLimitsView renders the per-tenant rate-limit override page.
type RateLimitsView struct {
	UserEmail       string
	CSRFToken       string
	TenantID        int64
	IsPlatformAdmin bool
	// API HTTP limit (tenant-wide, platform-admin editable).
	APIOverridden   bool
	APIRate         float64
	APIBurst        float64
	APIDefaultRate  float64
	APIDefaultBurst float64
	// Per-project invite quotas (tenant-admin editable).
	Projects           []ProjectInviteLimitView
	DefaultInviterHour float64
	DefaultDomainDay   float64
	// Recipient invite limit (tenant-wide, platform-admin editable): how many
	// back-to-back invites may go to the same address (burst) and the cooldown
	// window (seconds) that gates them. RecipientOverridden is false when the
	// compiled defaults apply.
	RecipientOverridden          bool
	RecipientBurst               float64
	RecipientCooldownSecs        float64
	DefaultRecipientBurst        float64
	DefaultRecipientCooldownSecs float64
	// Storage object value-size cap in bytes. StoragePlatformDefault is the
	// config fallback; StorageTenantOverride (0 = none) is platform-admin
	// editable; per-project overrides live on each ProjectInviteLimitView.
	StoragePlatformDefault int64
	StorageTenantOverride  int64
	Message                string
	Error                  string
}

// ProjectInviteLimitView is one project's invite-quota override (0 = default)
// plus its storage value-size override in bytes (0 = default).
type ProjectInviteLimitView struct {
	ProjectID            int64
	ProjectName          string
	InviterPerHour       float64
	DomainPerDay         float64
	StorageOverrideBytes int64
}

// APILimitCardView is the tenant HTTP API limit card shared by the
// rate-limits and tenant-settings pages. RedirectTo, when non-empty, is
// posted as redirect_to so the save returns to the embedding page.
type APILimitCardView struct {
	TenantID        int64
	CSRFToken       string
	RedirectTo      string
	IsPlatformAdmin bool
	Overridden      bool
	Rate            float64
	Burst           float64
	DefaultRate     float64
	DefaultBurst    float64
}

// apiLimitCardView adapts the rate-limits view to the shared card.
func (v RateLimitsView) apiLimitCardView() APILimitCardView {
	return APILimitCardView{
		TenantID:        v.TenantID,
		CSRFToken:       v.CSRFToken,
		IsPlatformAdmin: v.IsPlatformAdmin,
		Overridden:      v.APIOverridden,
		Rate:            v.APIRate,
		Burst:           v.APIBurst,
		DefaultRate:     v.APIDefaultRate,
		DefaultBurst:    v.APIDefaultBurst,
	}
}

// apiLimitCardView adapts the tenant-settings view to the shared card,
// returning the save to the settings page.
func (v TenantSettingsView) apiLimitCardView() APILimitCardView {
	return APILimitCardView{
		TenantID:        v.TenantID,
		CSRFToken:       v.CSRFToken,
		RedirectTo:      tenantSettingsPathTpl(v.TenantID),
		IsPlatformAdmin: v.IsPlatformAdmin,
		Overridden:      v.APIOverridden,
		Rate:            v.APIRate,
		Burst:           v.APIBurst,
		DefaultRate:     v.APIDefaultRate,
		DefaultBurst:    v.APIDefaultBurst,
	}
}

// HelpView is the data rendered by the in-app concepts page.
type HelpView struct {
	UserEmail string
	CSRFToken string
}

// TenantSettingsView consolidates tenant-scoped configuration on one page.
type TenantSettingsView struct {
	UserEmail       string
	CSRFToken       string
	TenantID        int64
	TenantName      string
	Tier            string
	IsPlatformAdmin bool
	Message         string
	// Tenant HTTP API limit (editable by platform admins, read-only otherwise).
	APIOverridden   bool
	APIRate         float64
	APIBurst        float64
	APIDefaultRate  float64
	APIDefaultBurst float64
}

// ProjectSettingsView consolidates project-scoped configuration on one page.
type ProjectSettingsView struct {
	UserEmail   string
	CSRFToken   string
	TenantID    int64
	ProjectID   int64
	ProjectName string
	CreatedAt   time.Time
	Message     string
	// Per-project invite quotas (editable, 0 = default).
	InviterPerHour     float64
	DomainPerDay       float64
	DefaultInviterHour float64
	DefaultDomainDay   float64
}

// ServerSettingsView renders the read-only server settings page.
type ServerSettingsView struct {
	UserEmail string
	CSRFToken string
	Snapshot  ServerSettingsSnapshot
}

// AccountView is the data rendered by the control panel account page.
type AccountView struct {
	UserEmail   string
	CSRFToken   string
	Message     string
	Error       string
	FieldErrors map[string]string
	// TwoFactorAvailable is false when the server has no TWO_FACTOR_ENC_KEY;
	// the 2FA card then explains enrollment is off instead of offering it.
	TwoFactorAvailable   bool
	TwoFactorEnabled     bool
	BackupCodesRemaining int
}

// TwoFactorChallengeView is the data rendered by the login TOTP challenge.
type TwoFactorChallengeView struct {
	Error string
}

// TwoFactorSetupView is the data rendered by the 2FA enrollment page.
type TwoFactorSetupView struct {
	UserEmail string
	CSRFToken string
	// QRDataURI is a server-generated PNG data URI; both CSPs allow
	// img-src data:.
	QRDataURI string
	// Secret is the base32 secret in display grouping for manual entry.
	Secret string
	Error  string
}

// TwoFactorBackupCodesView shows freshly generated backup codes, once.
type TwoFactorBackupCodesView struct {
	UserEmail string
	CSRFToken string
	Message   string
	Codes     []string
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

// PlatformUsersView is the data rendered by /v1/control-panel/admin/users.
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

// PlayerAccountsView is the data rendered by /v1/control-panel/admin/player-accounts.
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

// MatchmakerBucketView is one (mode, region, game_mode, status) bucket row.
// MinCount/MaxCount are the spread across the bucket's tickets.
type MatchmakerBucketView struct {
	Mode     string
	Region   string
	GameMode string
	Status   string
	Count    int64
	Oldest   time.Time
	MinCount int
	MaxCount int
}

// MatchmakerMatchCountView is the matches-formed count for one mode within
// the match retention window.
type MatchmakerMatchCountView struct {
	Mode  string
	Count int64
}

// MatchmakerQueueView is the data rendered by the matchmaker queue page.
type MatchmakerQueueView struct {
	UserEmail string
	CSRFToken string
	TenantID  int64
	ProjectID int64
	Buckets   []MatchmakerBucketView
	Matches   []MatchmakerMatchCountView
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

// LeaderboardsListView renders a project's leaderboard list page.
type LeaderboardsListView struct {
	UserEmail    string
	CSRFToken    string
	TenantID     int64
	ProjectID    int64
	Leaderboards []LeaderboardRowView
	Message      string
}

// LeaderboardRowView is one leaderboard row in the list.
type LeaderboardRowView struct {
	ID        int64
	Name      string
	SortOrder string
	CreatedAt time.Time
}

// LeaderboardFormView renders the create- and edit-leaderboard forms.
// LeaderboardID is zero on create.
type LeaderboardFormView struct {
	UserEmail     string
	CSRFToken     string
	TenantID      int64
	ProjectID     int64
	LeaderboardID int64
	Name          string
	SortOrder     string
	Error         string
	FieldErrors   map[string]string
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
	// The explicit suffix keeps operators in other timezones from reading
	// the value as local time.
	return t.UTC().Format("15:04 2006/01/02") + " UTC"
}

// orDash renders an em dash for empty config values on the read-only server
// settings page.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func joinComma(s []string) string {
	return strings.Join(s, ", ")
}

// tenantSettingsPathTpl / projectSettingsPathTpl build the redirect_to targets
// so a reused settings POST (e.g. rate-limits) returns to the settings page.
func tenantSettingsPathTpl(tenantID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) + "/settings"
}

func projectSettingsPathTpl(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/settings"
}

func csrfHeaders(token string) string {
	body, err := json.Marshal(map[string]string{csrfHeader: token})
	if err != nil {
		return "{}"
	}
	return string(body)
}
