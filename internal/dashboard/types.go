// Package dashboard implements the M1 server-rendered admin dashboard.
package dashboard

import (
	"encoding/json"
	"strconv"
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
	UserEmail   string
	CSRFToken   string
	TenantID    int64
	Projects    []ProjectOption
	Label       string
	ProjectID   string
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
}

func stringFromInt(n int64) string {
	return strconv.FormatInt(n, 10)
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
