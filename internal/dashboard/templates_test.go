package dashboard

import (
	"bytes"
	"context"
	"testing"

	"github.com/a-h/templ"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renderToString(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, c.Render(context.Background(), &buf))
	return buf.String()
}

func TestLoginPage_RendersFieldErrors(t *testing.T) {
	html := renderToString(t, LoginPage(LoginView{
		Email:       "bob@example.com",
		FieldErrors: map[string]string{"email": "Enter a valid email address"},
	}))
	assert.Contains(t, html, `class="field-error"`)
	assert.Contains(t, html, `id="email-error"`)
	assert.Contains(t, html, "Enter a valid email address")
	assert.NotContains(t, html, `id="password-error"`, "no error means no field-error element")
}

func TestSignupSuccessPage_WrapsRevealInLimeColorBlock(t *testing.T) {
	html := renderToString(t, SignupSuccessPage(SignupSuccessView{
		TenantID: 1, ProjectID: 2, APIKeyID: 3, APIKey: "secret",
	}))
	assert.Contains(t, html, `<section class="color-block color-block--lime">`)
}

func TestAPIKeyCreatedPage_WrapsRevealInLimeColorBlock(t *testing.T) {
	html := renderToString(t, APIKeyCreatedPage(SignupSuccessView{
		TenantID: 7, APIKeyID: 9, APIKey: "secret",
	}))
	assert.Contains(t, html, `<section class="color-block color-block--lime">`)
}

func TestNewTenantPage_HasCSRFHeaderForHTMX(t *testing.T) {
	html := renderToString(t, NewTenantPage(NewTenantView{
		UserEmail: "bob@example.com",
		CSRFToken: "tok-xyz",
	}))
	assert.Contains(t, html, "hx-post=")
	assert.Contains(t, html, "hx-headers=")
	assert.Contains(t, html, "X-CSRF-Token")
	assert.Contains(t, html, "tok-xyz")
}

func TestFormErrorFragment_RendersAlertRole(t *testing.T) {
	html := renderToString(t, FormErrorFragment("nope"))
	assert.Contains(t, html, `role="alert"`)
	assert.Contains(t, html, ">nope<")
}

func TestSetupTokenPage_RendersTokenFilePath(t *testing.T) {
	html := renderToString(t, SetupTokenPage(SetupTokenView{
		TokenFilePath: "/var/lib/ggscale/bootstrap.token",
	}))
	assert.Contains(t, html, "<code>/var/lib/ggscale/bootstrap.token</code>")
}

func TestSetupTokenPage_NoFilePathShowsStderrInstruction(t *testing.T) {
	html := renderToString(t, SetupTokenPage(SetupTokenView{}))
	assert.Contains(t, html, "DASHBOARD_BOOTSTRAP_TOKEN_FILE")
	assert.Contains(t, html, "stderr")
}

func TestSetupTokenPage_RendersFieldError(t *testing.T) {
	html := renderToString(t, SetupTokenPage(SetupTokenView{
		FieldErrors: map[string]string{"bootstrap_token": "Invalid bootstrap token"},
	}))
	assert.Contains(t, html, `id="bootstrap_token-error"`)
	assert.Contains(t, html, "Invalid bootstrap token")
}

func TestSetupTokenPage_PostsToTokenEndpoint(t *testing.T) {
	html := renderToString(t, SetupTokenPage(SetupTokenView{}))
	assert.Contains(t, html, `action="/v1/dashboard/setup/token"`)
}

func TestSetupAdminPage_HasHiddenTokenField(t *testing.T) {
	html := renderToString(t, SetupAdminPage(SetupAdminView{Token: "abc"}))
	assert.Contains(t, html, `<input type="hidden" name="bootstrap_token" value="abc"`)
}

func TestSetupAdminPage_PostsToSetupEndpoint(t *testing.T) {
	html := renderToString(t, SetupAdminPage(SetupAdminView{Token: "abc"}))
	assert.Contains(t, html, `action="/v1/dashboard/setup"`)
}

func TestSetupAdminPage_RendersFieldErrors(t *testing.T) {
	html := renderToString(t, SetupAdminPage(SetupAdminView{
		Token: "abc",
		Email: "bob@example.com",
		FieldErrors: map[string]string{
			"email":    "Enter a valid email address",
			"password": "Password must be at least 12 characters",
		},
	}))
	assert.Contains(t, html, `id="email-error"`)
	assert.Contains(t, html, "Enter a valid email address")
	assert.Contains(t, html, `id="password-error"`)
	assert.Contains(t, html, "Password must be at least 12 characters")
	assert.Contains(t, html, `class="field-error"`)
}

func TestSetupAdminPage_DoesNotShowTokenFilePath(t *testing.T) {
	html := renderToString(t, SetupAdminPage(SetupAdminView{Token: "abc"}))
	assert.NotContains(t, html, "DASHBOARD_BOOTSTRAP_TOKEN_FILE")
}
