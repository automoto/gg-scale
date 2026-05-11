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
	}, "alice@example.com", "tok"))
	assert.Contains(t, html, `<section class="color-block color-block--lime">`)
	assert.Contains(t, html, "Save your API key")
	assert.Contains(t, html, "alice@example.com")
}

func TestAPIKeyCreatedPage_LinksBackToList(t *testing.T) {
	html := renderToString(t, APIKeyCreatedPage(SignupSuccessView{
		TenantID: 7, APIKeyID: 9, APIKey: "secret",
	}, "alice@example.com", "tok"))
	assert.Contains(t, html, `href="/v1/dashboard/tenants/7/api-keys"`)
}

func TestProjectsPage_HasNewProjectButtonInHeader(t *testing.T) {
	html := renderToString(t, ProjectsPage(ProjectsView{
		UserEmail: "alice@example.com",
		TenantID:  42,
		CSRFToken: "tok",
	}))
	assert.Contains(t, html, `href="/v1/dashboard/tenants/42/projects/new"`)
	assert.Contains(t, html, "+ New project")
	assert.NotContains(t, html, `<h2>Create project</h2>`, "the inline create form moved to its own page")
}

func TestProjectsPage_RendersFlashMessage(t *testing.T) {
	html := renderToString(t, ProjectsPage(ProjectsView{
		UserEmail: "alice@example.com",
		TenantID:  42,
		Message:   "Project \"arcade-prod\" created.",
	}))
	assert.Contains(t, html, `class="flash-success"`)
	assert.Contains(t, html, `Project &#34;arcade-prod&#34; created.`)
}

func TestNewProjectPage_RendersFormWithCSRFAndCancelLink(t *testing.T) {
	html := renderToString(t, NewProjectPage(NewProjectView{
		UserEmail: "alice@example.com",
		CSRFToken: "tok-xyz",
		TenantID:  42,
	}))
	assert.Contains(t, html, `action="/v1/dashboard/tenants/42/projects"`)
	assert.Contains(t, html, `<input type="hidden" name="_csrf" value="tok-xyz">`)
	assert.Contains(t, html, `href="/v1/dashboard/tenants/42/projects"`, "cancel link returns to list")
}

func TestNewProjectPage_RendersFieldErrorAndPreservesInput(t *testing.T) {
	html := renderToString(t, NewProjectPage(NewProjectView{
		UserEmail:   "alice@example.com",
		TenantID:    42,
		Name:        "arcade prod",
		FieldErrors: map[string]string{"name": "Project name is required"},
	}))
	assert.Contains(t, html, "Project name is required")
	assert.Contains(t, html, `value="arcade prod"`)
}

func TestAPIKeysPage_HasNewAPIKeyButtonInHeader(t *testing.T) {
	html := renderToString(t, APIKeysPage(APIKeysView{
		UserEmail: "alice@example.com",
		TenantID:  42,
	}))
	assert.Contains(t, html, `href="/v1/dashboard/tenants/42/api-keys/new"`)
	assert.Contains(t, html, "+ New API key")
	assert.NotContains(t, html, "create-toggle", "the inline create-toggle moved to its own page")
}

func TestNewAPIKeyPage_RendersProjectSelectAndPreservesSelection(t *testing.T) {
	html := renderToString(t, NewAPIKeyPage(NewAPIKeyView{
		UserEmail: "alice@example.com",
		CSRFToken: "tok",
		TenantID:  42,
		Projects: []ProjectOption{
			{ID: 1, Name: "arcade-prod"},
			{ID: 2, Name: "arcade-staging"},
		},
		ProjectID: "2",
		Label:     "ci-key",
	}))
	assert.Contains(t, html, `action="/v1/dashboard/tenants/42/api-keys"`)
	assert.Contains(t, html, `<option value="2" selected>arcade-staging</option>`)
	assert.Contains(t, html, `<option value="1">arcade-prod</option>`)
	assert.Contains(t, html, `value="ci-key"`)
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
