package controlpanel

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

func TestUniqueViolationConstraint(t *testing.T) {
	name, ok := uniqueViolationConstraint(&pgconn.PgError{Code: "23505", ConstraintName: "tenant_signup_requests_live_name_key"})
	assert.True(t, ok)
	assert.Equal(t, "tenant_signup_requests_live_name_key", name)

	_, ok = uniqueViolationConstraint(&pgconn.PgError{Code: "23503"})
	assert.False(t, ok, "non-unique-violation is not a constraint hit")

	_, ok = uniqueViolationConstraint(errors.New("plain error"))
	assert.False(t, ok)
}

func TestSignupEnabledOrClosed(t *testing.T) {
	t.Run("missing singleton row defaults to closed", func(t *testing.T) {
		// An unseeded platform_signup_config makes GetPublicSignupEnabled return
		// pgx.ErrNoRows; the admin page must fall back to disabled, not 500.
		enabled, err := signupEnabledOrClosed(false, pgx.ErrNoRows)
		assert.NoError(t, err)
		assert.False(t, enabled)
	})

	t.Run("configured value passes through", func(t *testing.T) {
		enabled, err := signupEnabledOrClosed(true, nil)
		assert.NoError(t, err)
		assert.True(t, enabled)
	})

	t.Run("real error propagates", func(t *testing.T) {
		boom := errors.New("connection reset")
		_, err := signupEnabledOrClosed(false, boom)
		assert.ErrorIs(t, err, boom)
	})
}

func TestValidTenantName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"one char", "a", false},
		{"two chars ok", "Ab", true},
		{"trims to empty", "   ", false},
		{"max length ok", strings.Repeat("x", tenantNameMax), true},
		{"over max", strings.Repeat("x", tenantNameMax+1), false},
		{"control char rejected", "ab\tcd", false},
		{"normal name", "Abyssal Depths", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, validTenantName(c.in))
		})
	}
}

func TestValidateTenantSignupInput(t *testing.T) {
	valid := tenantSignupInput{
		Email:               "dev@example.com",
		RequestedTenantName: "Abyssal Depths",
		ProjectDescription:  "A co-op roguelike diver.",
	}

	t.Run("valid input has no errors", func(t *testing.T) {
		assert.Empty(t, validateTenantSignupInput(valid))
	})

	t.Run("bad email", func(t *testing.T) {
		in := valid
		in.Email = "not-an-email"
		assert.Contains(t, validateTenantSignupInput(in), "email")
	})

	t.Run("bad tenant name", func(t *testing.T) {
		in := valid
		in.RequestedTenantName = "x"
		assert.Contains(t, validateTenantSignupInput(in), "tenant_name")
	})

	t.Run("empty description", func(t *testing.T) {
		in := valid
		in.ProjectDescription = ""
		assert.Contains(t, validateTenantSignupInput(in), "project_description")
	})

	t.Run("over-long description", func(t *testing.T) {
		in := valid
		in.ProjectDescription = strings.Repeat("x", projectDescriptionMax+1)
		assert.Contains(t, validateTenantSignupInput(in), "project_description")
	})

	t.Run("over-long studio name", func(t *testing.T) {
		in := valid
		in.StudioName = strings.Repeat("x", studioNameMax+1)
		assert.Contains(t, validateTenantSignupInput(in), "studio_name")
	})
}

func TestTenantSignupPage_renders_form(t *testing.T) {
	html := renderToString(t, TenantSignupPage(TenantSignupFormView{CSRFToken: "tok"}))

	assert.Contains(t, html, `action="/v1/control-panel/request-access"`)
	assert.Contains(t, html, `name="tenant_name"`)
	assert.Contains(t, html, `name="email"`)
	assert.Contains(t, html, `name="project_description"`)
	assert.Contains(t, html, `name="studio_name"`)
	assert.Contains(t, html, `name="_csrf" value="tok"`)
}

func TestTenantSignupPage_shows_field_errors(t *testing.T) {
	html := renderToString(t, TenantSignupPage(TenantSignupFormView{
		CSRFToken:   "tok",
		TenantName:  "x",
		FieldErrors: map[string]string{"tenant_name": "Tenant name must be 2–60 characters."},
	}))

	assert.Contains(t, html, "Tenant name must be")
}

func TestTenantSignupClosedPage_says_closed(t *testing.T) {
	html := renderToString(t, TenantSignupClosedPage())

	assert.Contains(t, html, "closed")
	assert.NotContains(t, html, `name="tenant_name"`)
}

func TestTenantSignupAcknowledgePage_is_generic(t *testing.T) {
	html := renderToString(t, TenantSignupAcknowledgePage())

	// Anti-enumeration: a fixed acknowledgement that reveals nothing about
	// whether the email already applied.
	assert.Contains(t, html, "Request received")
	assert.NotContains(t, html, "already")
}

func TestTenantSignupRequestsPage_enabled_shows_disable_and_requests(t *testing.T) {
	html := renderToString(t, TenantSignupRequestsPage(TenantSignupRequestsView{
		CSRFToken:     "tok",
		SignupEnabled: true,
		Requests: []TenantSignupRequestView{{
			ID: 7, Email: "dev@example.com", TenantName: "Abyssal Depths",
			ProjectDescription: "A co-op roguelike.", CreatedAt: time.Now(),
		}},
	}))

	assert.Contains(t, html, "Disable public sign-up")
	assert.Contains(t, html, `action="/v1/control-panel/admin/tenant-signups/config"`)
	assert.Contains(t, html, "Abyssal Depths")
	assert.Contains(t, html, `/v1/control-panel/admin/tenant-signups/7/approve`)
	assert.Contains(t, html, `/v1/control-panel/admin/tenant-signups/7/deny`)
}

func TestTenantSignupRequestsPage_disabled_shows_enable(t *testing.T) {
	html := renderToString(t, TenantSignupRequestsPage(TenantSignupRequestsView{
		CSRFToken:     "tok",
		SignupEnabled: false,
	}))

	assert.Contains(t, html, "Enable public sign-up")
	assert.Contains(t, html, "No pending requests")
}

func TestTenantSignupAcceptPage_new_user_sets_password(t *testing.T) {
	html := renderToString(t, TenantSignupAcceptPage(TenantSignupAcceptView{
		CSRFToken: "tok", Email: "dev@example.com", TenantName: "Abyssal Depths", NewUser: true,
	}))

	assert.Contains(t, html, "Set a password")
	assert.Contains(t, html, "Abyssal Depths")
	assert.Contains(t, html, `name="_csrf" value="tok"`)
}

func TestTenantSignupAcceptPage_existing_user_confirms_password(t *testing.T) {
	html := renderToString(t, TenantSignupAcceptPage(TenantSignupAcceptView{
		CSRFToken: "tok", Email: "dev@example.com", TenantName: "Abyssal Depths", NewUser: false,
	}))

	assert.Contains(t, html, "existing password")
}

func TestTenantSignupAcceptPage_error_hides_form(t *testing.T) {
	html := renderToString(t, TenantSignupAcceptPage(TenantSignupAcceptView{
		CSRFToken: "tok", Error: "Invite not found or already used.",
	}))

	assert.Contains(t, html, "Invite not found")
	assert.NotContains(t, html, `name="password"`)
}
