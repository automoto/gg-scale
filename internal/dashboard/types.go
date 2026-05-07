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

// SetupView is the data rendered by the first-run setup template.
type SetupView struct {
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
	UserEmail string
	CSRFToken string
	Tenants   []TenantView
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
	UserEmail   string
	TenantID    int64
	CSRFToken   string
	Projects    []ProjectOption
	Error       string
	Message     string
	FieldErrors map[string]string
}

// APIKeysView is the data rendered by the API-key management page.
type APIKeysView struct {
	UserEmail   string
	TenantID    int64
	CSRFToken   string
	Keys        []APIKeyView
	Projects    []ProjectOption
	Message     string
	Error       string
	FieldErrors map[string]string
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
