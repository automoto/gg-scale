package controlpanel

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

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

func TestTimeString_marks_values_as_utc(t *testing.T) {
	at := time.Date(2026, 7, 6, 14, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	assert.Equal(t, "12:30 2026/07/06 UTC", timeString(at))
	assert.Equal(t, "-", timeString(time.Time{}))
}

func TestPlayersPage_LabelsNonFinalPageAsApproximate(t *testing.T) {
	html := renderToString(t, PlayersPage(PlayersView{
		UserEmail: "alice@example.com",
		TenantID:  1,
		ProjectID: 2,
		Players:   []PlayerView{{ID: 3, Email: "player@example.com"}},
		Total:     26,
		HasNext:   true,
	}))

	assert.Contains(t, html, "Showing 1 of at least 26.")
}

func TestAccountPage_TwoFactorCardStates(t *testing.T) {
	tests := []struct {
		name        string
		vm          AccountView
		contains    string
		notContains string
	}{
		{
			name:        "should_offer_enable_when_available_and_not_enrolled",
			vm:          AccountView{TwoFactorAvailable: true},
			contains:    "/v1/control-panel/account/2fa/setup",
			notContains: "/v1/control-panel/account/2fa/disable",
		},
		{
			name:        "should_offer_disable_and_regenerate_when_enrolled",
			vm:          AccountView{TwoFactorAvailable: true, TwoFactorEnabled: true, BackupCodesRemaining: 7},
			contains:    "/v1/control-panel/account/2fa/disable",
			notContains: "/v1/control-panel/account/2fa/setup",
		},
		{
			name:        "should_explain_when_server_has_no_key",
			vm:          AccountView{},
			contains:    "Not available on this server",
			notContains: "/v1/control-panel/account/2fa/setup",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := renderToString(t, AccountPage(tt.vm))

			assert.Contains(t, html, tt.contains)
			assert.NotContains(t, html, tt.notContains)
		})
	}
}

func TestAccountPage_ShowsBackupCodesRemaining(t *testing.T) {
	html := renderToString(t, AccountPage(AccountView{TwoFactorAvailable: true, TwoFactorEnabled: true, BackupCodesRemaining: 7}))

	assert.Contains(t, html, "Backup codes remaining: 7.")
}

func TestTwoFactorSetupPage_RendersQRAndSecret(t *testing.T) {
	html := renderToString(t, TwoFactorSetupPage(TwoFactorSetupView{
		QRDataURI: "data:image/png;base64,AAAA",
		Secret:    "ABCD EFGH",
	}))

	assert.Contains(t, html, `src="data:image/png;base64,AAAA"`)
	assert.Contains(t, html, "ABCD EFGH")
	assert.Contains(t, html, "/v1/control-panel/account/2fa/confirm")
}

func TestTwoFactorChallengePage_HasTrustDeviceAndNoCSRF(t *testing.T) {
	html := renderToString(t, TwoFactorChallengePage(TwoFactorChallengeView{Error: "That code is incorrect."}))

	assert.Contains(t, html, `name="trust_device"`)
	assert.Contains(t, html, `name="code"`)
	assert.Contains(t, html, "That code is incorrect.")
	// Mirrors the login POST: pre-session, no CSRF field.
	assert.NotContains(t, html, `name="_csrf"`)
}

func TestTwoFactorBackupCodesPage_ListsCodes(t *testing.T) {
	html := renderToString(t, TwoFactorBackupCodesPage(TwoFactorBackupCodesView{
		Codes: []string{"abc23-def45", "ghj67-klm23"},
	}))

	assert.Contains(t, html, "abc23-def45")
	assert.Contains(t, html, "ghj67-klm23")
	assert.Contains(t, html, "shown again")
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

func TestVerifyPage_SeparatesPrimaryAndResendActions(t *testing.T) {
	html := renderToString(t, VerifyPage(VerifyView{Email: "admin@example.com"}))

	// The primary Verify action uses the plain full-width button like the
	// login page, not the cramped right-aligned .form-actions/.btn-inline
	// row (which sat the small Verify button flush against the resend form).
	assert.NotContains(t, html, "form-actions")
	assert.NotContains(t, html, "btn-inline")
	// The resend form is clearly separated from the Verify form.
	assert.Contains(t, html, `class="resend-form"`)
	assert.Contains(t, html, ">Verify</button>")
	assert.Contains(t, html, ">Send a new code</button>")
}

func TestControlPanelHeadUsesExternalScriptsAndSafeHTMXConfig(t *testing.T) {
	html := renderToString(t, LoginPage(LoginView{}))

	assert.Contains(t, html, `name="htmx-config"`)
	assert.Contains(t, html, "includeIndicatorStyles")
	assert.Contains(t, html, "allowEval")
	assert.Contains(t, html, "allowScriptTags")
	assert.Contains(t, html, `src="/v1/control-panel/assets/htmx.min.js?v=`)
	assert.Contains(t, html, `src="/v1/control-panel/assets/controlpanel.js?v=`)
	assert.NotContains(t, html, `unsafe-inline`)
}

func TestControlPanelConfirmFormsUseDataAttributes(t *testing.T) {
	tests := []struct {
		name string
		page templ.Component
	}{
		{
			name: "team",
			page: TeamPage(TeamView{
				TenantID:  1,
				CSRFToken: "tok",
				Members: []TeamMemberView{
					{MembershipID: 10, UserID: 20, Email: "member@example.com"},
				},
				Pending: []PendingInviteView{
					{ID: 30, Email: "pending@example.com"},
				},
			}),
		},
		{
			name: "platform users",
			page: PlatformUsersPage(PlatformUsersView{
				CSRFToken: "tok",
				Users: []UserView{
					{ID: 40, Email: "user@example.com"},
				},
			}),
		},
		{
			name: "edit fleet",
			page: EditFleetPage(EditFleetView{
				TenantID: 1, ProjectID: 2, FleetID: 5, Name: "primary",
				Backend: "docker", BackendConfigured: "docker",
				Config: map[string]string{"image": "x:1", "port": "80"},
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			html := renderToString(t, tt.page)
			assert.NotContains(t, html, `onsubmit=`)
			assert.Contains(t, html, `data-confirm=`)
		})
	}
}

func TestHomePage_RendersPlatformMenuWhenAuthorized(t *testing.T) {
	html := renderToString(t, HomePage(HomeView{
		UserEmail:       "admin@example.com",
		CSRFToken:       "tok",
		IsPlatformAdmin: true,
	}))

	assert.Contains(t, html, "<summary>Platform</summary>")
	assert.Contains(t, html, `href="/v1/control-panel/admin/users"`)
	assert.Contains(t, html, `href="/v1/control-panel/admin/player-accounts"`)
	assert.Contains(t, html, `aria-current="page">Tenants</a>`)
}

func TestHomePage_HidesPlatformMenuWhenNotAuthorized(t *testing.T) {
	html := renderToString(t, HomePage(HomeView{UserEmail: "user@example.com"}))

	assert.NotContains(t, html, "<summary>Platform</summary>")
	assert.NotContains(t, html, `href="/v1/control-panel/admin/users"`)
}

func TestProjectsPage_RendersTenantNavigationAndNoSecondaryHeaderButtons(t *testing.T) {
	html := renderToString(t, ProjectsPage(ProjectsView{
		UserEmail: "alice@example.com",
		TenantID:  42,
		CSRFToken: "tok",
	}))

	assert.Contains(t, html, "<summary>Tenant</summary>")
	assert.Contains(t, html, `href="/v1/control-panel/tenants/42/api-keys"`)
	assert.Contains(t, html, `aria-current="page">Projects</a>`)
	assert.NotContains(t, html, `role="button" class="secondary outline btn-inline">Settings</a>`)
	assert.NotContains(t, html, `role="button" class="secondary outline btn-inline">Rate limits</a>`)
}

func TestPlayersPage_RendersProjectNavigationAndCompactTableAction(t *testing.T) {
	html := renderToString(t, PlayersPage(PlayersView{
		UserEmail: "alice@example.com",
		TenantID:  42,
		ProjectID: 7,
		CSRFToken: "tok",
		Players:   []PlayerView{{ID: 9, Email: "p@example.com"}},
	}))

	assert.Contains(t, html, "<summary>Project</summary>")
	assert.Contains(t, html, `aria-current="page">Players</a>`)
	assert.Contains(t, html, `class="filter-form"`)
	assert.Contains(t, html, `role="button" class="secondary outline btn-inline">View</a>`)
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
	assert.Contains(t, html, `href="/v1/control-panel/tenants/7/api-keys"`)
}

func TestProjectsPage_HasNewProjectButtonInHeader(t *testing.T) {
	html := renderToString(t, ProjectsPage(ProjectsView{
		UserEmail: "alice@example.com",
		TenantID:  42,
		CSRFToken: "tok",
	}))
	assert.Contains(t, html, `href="/v1/control-panel/tenants/42/projects/new"`)
	assert.Contains(t, html, "+ New project")
	assert.NotContains(t, html, `<h2>Create project</h2>`, "the inline create form moved to its own page")
}

func TestAPIKeysPage_feature_dialog_reflects_grantability(t *testing.T) {
	vm := APIKeysView{
		UserEmail: "alice@example.com",
		TenantID:  42,
		CSRFToken: "tok",
		Keys: []APIKeyView{
			{ID: 1, Label: "granted-fleet", Scopes: []string{"fleet"}, FleetGrantable: true},
			{ID: 2, Label: "grantable-relay", RelayGrantable: true},
			{ID: 3, Label: "no-access"},
		},
	}
	html := renderToString(t, APIKeysPage(vm))

	assert.NotContains(t, html, "Grant Matchmaker")
	assert.NotContains(t, html, "Grant Fleet")
	assert.NotContains(t, html, "Grant Relay")
	assert.Contains(t, html, `hx-get="/v1/control-panel/tenants/42/api-keys/1/features"`)
	assert.Contains(t, html, `hx-target="#modal-root"`)
	assert.NotContains(t, html, "<dialog")
	assert.Contains(t, html, "Manage features")
}

func TestAPIKeyFeaturesDialog_reflects_grantability(t *testing.T) {
	html := renderToString(t, APIKeyFeaturesDialog(42, "tok", APIKeyView{
		ID: 2, Label: "grantable-relay", Scopes: []string{"fleet"}, RelayGrantable: true,
	}))

	assert.Contains(t, html, `<dialog class="feature-dialog">`)
	assert.Contains(t, html, `/api-keys/2/features"`)
	assert.Contains(t, html, "Dedicated servers")
	assert.Contains(t, html, `value="fleet" checked`)
	assert.Contains(t, html, `value="p2p_relay"`)
	assert.Contains(t, html, "Available")
	assert.Contains(t, html, "Not available for this project")
}

func TestAPIKeysPage_revocation_has_specific_confirmation(t *testing.T) {
	html := renderToString(t, APIKeysPage(APIKeysView{
		UserEmail: "alice@example.com",
		TenantID:  42,
		CSRFToken: "tok",
		Keys:      []APIKeyView{{ID: 1, Label: "key"}},
	}))

	assert.Contains(t, html, `data-confirm="Revoke this API key? Clients using it will immediately lose access."`)
}

func TestTeamPage_fleet_operator_control_gated_on_feature(t *testing.T) {
	members := []TeamMemberView{
		{MembershipID: 1, UserID: 10, Email: "op@example.com", Role: "member", FleetOperator: false},
		{MembershipID: 2, UserID: 11, Email: "boss@example.com", Role: "admin", FleetOperator: true},
	}

	off := renderToString(t, TeamPage(TeamView{TenantID: 42, CSRFToken: "tok", Members: members, FleetEnabled: false}))
	assert.NotContains(t, off, "/roles\"", "fleet-operator control hidden when feature off")
	assert.NotContains(t, off, "Fleet access")

	on := renderToString(t, TeamPage(TeamView{TenantID: 42, CSRFToken: "tok", Members: members, FleetEnabled: true}))
	assert.Contains(t, on, "Fleet access")
	assert.Contains(t, on, "/team/members/10/roles\"")
	assert.Contains(t, on, "Grant fleet operator")
	assert.Contains(t, on, "/team/members/11/roles\"")
	assert.Contains(t, on, "Fleet operator")
}

func TestRateLimitsPage_api_form_platform_admin_only(t *testing.T) {
	base := RateLimitsView{
		UserEmail: "a@example.com", TenantID: 5, CSRFToken: "tok",
		APIDefaultRate: 60, APIDefaultBurst: 60,
		DefaultInviterHour: 10, DefaultDomainDay: 100,
		Projects: []ProjectInviteLimitView{{ProjectID: 7, ProjectName: "arcade"}},
	}

	admin := base
	admin.IsPlatformAdmin = true
	adminHTML := renderToString(t, RateLimitsPage(admin))
	assert.Contains(t, adminHTML, `/tenants/5/rate-limits/api"`)
	assert.Contains(t, adminHTML, "Save API limit")
	assert.Contains(t, adminHTML, `/tenants/5/rate-limits/projects/7/invites"`)

	tenantAdmin := base
	tenantAdmin.IsPlatformAdmin = false
	taHTML := renderToString(t, RateLimitsPage(tenantAdmin))
	assert.NotContains(t, taHTML, "Save API limit", "tenant admin can't edit the API ceiling")
	assert.Contains(t, taHTML, "Only platform admins")
	// Tenant admins still get the per-project invite quota forms.
	assert.Contains(t, taHTML, `/tenants/5/rate-limits/projects/7/invites"`)
}

func TestProjectsPage_hides_fleet_actions_when_feature_off(t *testing.T) {
	vm := ProjectsView{
		UserEmail:    "alice@example.com",
		TenantID:     42,
		CSRFToken:    "tok",
		Projects:     []ProjectOption{{ID: 7, Name: "arcade-prod"}},
		FleetEnabled: false,
	}
	html := renderToString(t, ProjectsPage(vm))
	assert.NotContains(t, html, `/projects/7/fleets"`)
	assert.NotContains(t, html, `/projects/7/allocations"`)
}

func TestProjectsPage_shows_fleet_actions_when_feature_on(t *testing.T) {
	vm := ProjectsView{
		UserEmail:    "alice@example.com",
		TenantID:     42,
		CSRFToken:    "tok",
		Projects:     []ProjectOption{{ID: 7, Name: "arcade-prod"}},
		FleetEnabled: true,
	}
	html := renderToString(t, ProjectsPage(vm))
	assert.Contains(t, html, `/projects/7/fleets"`)
	assert.Contains(t, html, `/projects/7/allocations"`)
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
	assert.Contains(t, html, `action="/v1/control-panel/tenants/42/projects"`)
	assert.Contains(t, html, `<input type="hidden" name="_csrf" value="tok-xyz">`)
	assert.Contains(t, html, `href="/v1/control-panel/tenants/42/projects"`, "cancel link returns to list")
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
	assert.Contains(t, html, `href="/v1/control-panel/tenants/42/api-keys/new"`)
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
	assert.Contains(t, html, `action="/v1/control-panel/tenants/42/api-keys"`)
	assert.Contains(t, html, `<option value="2" selected>arcade-staging</option>`)
	assert.Contains(t, html, `<option value="1">arcade-prod</option>`)
	assert.Contains(t, html, `value="ci-key"`)
}

func TestNewAPIKeyPage_KeyTypeSelector(t *testing.T) {
	cases := []struct {
		name              string
		view              NewAPIKeyView
		publishableActive bool
		secretActive      bool
	}{
		{
			name: "publishable_default_when_keytype_unset",
			view: NewAPIKeyView{TenantID: 1, KeyType: ""},
			// Empty falls through to publishable-selected branch.
			publishableActive: true,
			secretActive:      false,
		},
		{
			name:              "publishable_explicit",
			view:              NewAPIKeyView{TenantID: 1, KeyType: "publishable"},
			publishableActive: true,
			secretActive:      false,
		},
		{
			name:              "secret_explicit",
			view:              NewAPIKeyView{TenantID: 1, KeyType: "secret"},
			publishableActive: false,
			secretActive:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			html := renderToString(t, NewAPIKeyPage(tc.view))
			assert.Contains(t, html, `name="key_type"`, "form must include key_type input")
			if tc.publishableActive {
				assert.Contains(t, html, `<option value="publishable" selected>`)
			}
			if tc.secretActive {
				assert.Contains(t, html, `<option value="secret" selected>`)
			}
		})
	}
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

func TestErrorAlert_RendersWhenMessagePresent(t *testing.T) {
	html := renderToString(t, errorAlert("boom"))
	assert.Contains(t, html, `<p role="alert">boom</p>`)
}

func TestErrorAlert_RendersNothingWhenEmpty(t *testing.T) {
	html := renderToString(t, errorAlert(""))
	assert.Empty(t, html)
}

func TestFlashSuccess_RendersWhenMessagePresent(t *testing.T) {
	html := renderToString(t, flashSuccess("saved"))
	assert.Contains(t, html, `<p class="flash-success">saved</p>`)
}

func TestFlashSuccess_RendersNothingWhenEmpty(t *testing.T) {
	html := renderToString(t, flashSuccess(""))
	assert.Empty(t, html)
}

func TestNewAPIKeyPage_MarksSelectedKeyType(t *testing.T) {
	html := renderToString(t, NewAPIKeyPage(NewAPIKeyView{
		KeyType: "secret",
	}))
	assert.Contains(t, html, `<option value="secret" selected>`)
	assert.NotContains(t, html, `<option value="publishable" selected>`)
}

func TestInviteTeamPage_MarksSelectedRoleOnce(t *testing.T) {
	html := renderToString(t, InviteTeamPage(InviteTeamView{
		Role: "tenant_member",
	}))
	// The invite form has a single <select>, so exactly one option is selected.
	assert.Equal(t, 1, strings.Count(html, " selected"))
	assert.Contains(t, html, `<option value="tenant_member" selected>`)
}

func TestSetupTokenPage_RendersTokenFilePath(t *testing.T) {
	html := renderToString(t, SetupTokenPage(SetupTokenView{
		TokenFilePath: "/var/lib/ggscale/bootstrap.token",
	}))
	assert.Contains(t, html, "<code>/var/lib/ggscale/bootstrap.token</code>")
}

func TestSetupTokenPage_NoFilePathShowsStderrInstruction(t *testing.T) {
	html := renderToString(t, SetupTokenPage(SetupTokenView{}))
	assert.Contains(t, html, "CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE")
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
	assert.Contains(t, html, `action="/v1/control-panel/setup/token"`)
}

func TestSetupAdminPage_HasHiddenTokenField(t *testing.T) {
	html := renderToString(t, SetupAdminPage(SetupAdminView{Token: "abc"}))
	assert.Contains(t, html, `<input type="hidden" name="bootstrap_token" value="abc"`)
}

func TestSetupAdminPage_PostsToSetupEndpoint(t *testing.T) {
	html := renderToString(t, SetupAdminPage(SetupAdminView{Token: "abc"}))
	assert.Contains(t, html, `action="/v1/control-panel/setup"`)
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
	assert.NotContains(t, html, "CONTROL_PANEL_BOOTSTRAP_TOKEN_FILE")
}

func TestPlayerDetail_renders_typed_remote_addrs_with_badges(t *testing.T) {
	html := renderToString(t, PlayerDetailPage(PlayerDetailView{
		Player: PlayerView{
			ID:        3,
			AccountID: "9f1c2d3e-0000-0000-0000-000000000000",
			RemoteAddrs: []RemoteAddrView{
				{TypeLabel: "LAN IP", ScopeLabel: "LAN", Address: "192.168.1.4:9000"},
				{TypeLabel: "DNS name", Address: "example.com:7777"},
			},
		},
	}))

	assert.Contains(t, html, "LAN IP")
	assert.Contains(t, html, "192.168.1.4:9000")
	assert.Contains(t, html, `<span class="badge">LAN</span>`)
	assert.Contains(t, html, "example.com:7777")
}

func TestPlayerDetail_shows_placeholder_when_no_remote_addrs(t *testing.T) {
	html := renderToString(t, PlayerDetailPage(PlayerDetailView{
		Player: PlayerView{ID: 3, AccountID: "9f1c2d3e-0000-0000-0000-000000000000"},
	}))

	assert.Contains(t, html, "Remote addresses")
	assert.Contains(t, html, "—")
}
