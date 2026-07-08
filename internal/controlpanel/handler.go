package controlpanel

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/twofactor"
	"github.com/ggscale/ggscale/internal/webassets"
	"github.com/ggscale/ggscale/internal/webutil"
)

//go:embed static
var staticFS embed.FS

// Deps groups everything controlpanel.New needs. Using a struct (instead
// of seven positional params) keeps the call site readable as
// dependencies grow.
type Deps struct {
	Pool    *db.Pool
	Cache   cache.Store
	Limiter ratelimit.Limiter
	// RateLimitOverrides (may be nil) supplies per-tenant/project invite-limit
	// overrides to the invite throttle.
	RateLimitOverrides ratelimit.OverrideStore
	// ProxyTrust resolves the real client IP for the per-IP auth limiter when
	// behind a trusted reverse proxy. nil = RemoteAddr only.
	ProxyTrust *ratelimit.ProxyTrust
	Registry   prometheus.Registerer
	// Metrics carries the business counters. nil is a no-op (unit tests).
	Metrics   *observability.Metrics
	Config    Config
	Bootstrap *Bootstrap
	Mailer    mailer.Mailer
	// Fleet is the manager the control panel reads allocations from and
	// invokes manual Allocate/Deallocate against. nil when no backend is
	// configured — fleet pages render "not configured" in that case.
	Fleet *fleet.Manager
	// RBAC is the Casbin authorizer. nil preserves the legacy control panel
	// checks for tests and during partial construction.
	RBAC *rbac.Authorizer
	// PluginInfo returns a snapshot of the running fleet plugin (if the
	// backend is a plugin). nil when not a plugin backend; the admin
	// plugins page renders "no plugin backend" in that case.
	PluginInfo func() *PluginSnapshot
	// TwoFactor encrypts TOTP secrets and signs the 2FA pending cookie.
	// nil = 2FA enrollment unavailable; already-enrolled logins fail closed.
	TwoFactor *twofactor.Cipher
}

// PluginSnapshot is the read-only view the admin/plugins page renders.
// Lives here so control panel does not need to import internal/fleet/plugin.
type PluginSnapshot struct {
	Name              string
	Version           string
	ProtocolVersion   int
	Pid               int
	RestartCount      int
	TotalRestartCount int
	HealthErr         string
}

// Handler owns control panel HTTP routes.
type Handler struct {
	pool           *db.Pool
	cache          cache.Store
	limiter        ratelimit.Limiter
	overrides      ratelimit.OverrideStore
	inviteThrottle *ratelimit.InviteThrottle
	reg            prometheus.Registerer
	cfg            Config
	bootstrap      *Bootstrap
	mailer         mailer.Mailer
	fleet          *fleet.Manager
	rbac           *rbac.Authorizer
	pluginInfo     func() *PluginSnapshot
	now            func() time.Time
	proxyTrust     *ratelimit.ProxyTrust
	metrics        *observability.Metrics
	// verifySigningKey signs the short-lived verify-pending cookie.
	// Generated once at handler construction so each process has a fresh
	// secret; restarts invalidate in-flight verify cookies (acceptable —
	// users re-enter from login).
	verifySigningKey []byte
	twoFactor        *twofactor.Cipher
}

// New builds the control panel router. Callers should only mount it when
// d.Config.Enabled returns true.
func New(d Deps) http.Handler {
	bootstrap := d.Bootstrap
	if bootstrap == nil {
		bootstrap = DisabledBootstrap()
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// crypto/rand failure is unrecoverable; this only fires at process
		// startup so panicking is appropriate.
		panic("control panel: rand: " + err.Error())
	}
	h := &Handler{
		pool:             d.Pool,
		cache:            d.Cache,
		limiter:          d.Limiter,
		overrides:        d.RateLimitOverrides,
		reg:              d.Registry,
		cfg:              d.Config,
		bootstrap:        bootstrap,
		mailer:           d.Mailer,
		fleet:            d.Fleet,
		rbac:             d.RBAC,
		pluginInfo:       d.PluginInfo,
		now:              time.Now,
		proxyTrust:       d.ProxyTrust,
		metrics:          d.Metrics,
		verifySigningKey: key,
		twoFactor:        d.TwoFactor,
	}
	if d.Limiter != nil && d.Registry != nil {
		h.inviteThrottle = ratelimit.NewInviteThrottle(d.Limiter, ratelimit.DefaultInviteLimits, d.Registry).
			WithOverrides(d.RateLimitOverrides)
	}

	r := chi.NewRouter()
	r.Use(webutil.SecurityHeaders)
	r.Get("/assets/*", h.assetHandler)
	r.Group(func(r chi.Router) {
		if d.Limiter != nil {
			r.Use(ratelimit.NewIPLimiter(d.Limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, d.ProxyTrust, d.Registry))
		}
		r.Get("/setup", h.setupTokenPage)
		r.Post("/setup/token", h.verifySetupToken)
		r.Post("/setup", h.completeSetup)
		r.Get("/login", h.loginPage)
		r.Post("/login", h.login)
		r.Get("/login/2fa", h.twoFactorChallengePage)
		r.Post("/login/2fa", h.twoFactorChallenge)
		r.Get("/verify", h.verifyPage)
		r.Post("/verify", h.verifyHandler)
		r.Post("/verify/resend", h.verifyResendHandler)
	})

	r.Group(func(r chi.Router) {
		r.Use(h.requireSession)
		r.Use(h.requireCSRF)
		r.Get("/", h.home)
		r.Get("/help", h.helpPage)
		r.Get("/tenants/new", h.newTenantPage)
		r.Post("/tenants", h.createTenantHandler)
		r.Get("/tenants", h.openTenant)
		r.Get("/account/password", h.accountPage)
		r.Post("/account/password", h.updatePassword)
		r.Post("/account/2fa/setup", h.twoFactorSetup)
		r.Post("/account/2fa/confirm", h.twoFactorConfirm)
		r.Post("/account/2fa/disable", h.twoFactorDisable)
		r.Post("/account/2fa/backup-codes", h.twoFactorRegenerateBackupCodes)
		r.Route("/tenants/{tenantID}", func(r chi.Router) {
			r.Use(h.requireTenantAccess(roleAdmin))
			r.Get("/projects", h.projectsPage)
			r.Get("/projects/new", h.newProjectPage)
			r.Post("/projects", h.createProjectHandler)
			r.Post("/public-joining", h.setTenantPublicJoiningHandler)
			r.Post("/projects/{projectID}/public-joining", h.setProjectPublicJoiningHandler)
			r.Get(segAPIKeys, h.apiKeys)
			r.Get("/api-keys/new", h.newAPIKeyPage)
			r.Post(segAPIKeys, h.createAPIKeyHandler)
			r.Post("/api-keys/{apiKeyID}/label", h.updateAPIKeyLabelHandler)
			r.Post("/api-keys/{apiKeyID}/scopes", h.updateAPIKeyScopesHandler)
			r.Post("/api-keys/{apiKeyID}/revoke", h.revokeAPIKeyHandler)
			r.Get("/rate-limits", h.rateLimitsPage)
			r.Post("/rate-limits/api", h.updateTenantAPILimitHandler)
			r.Post("/rate-limits/projects/{projectID}/invites", h.updateProjectInviteLimitHandler)
			r.Get("/team", h.teamPage)
			r.Get(segTeamInvite, h.inviteTeamPage)
			r.Post(segTeamInvite, h.inviteTeammateHandler)
			r.Post("/team/invites/{inviteID}/revoke", h.revokeInviteHandler)
			r.Post("/team/members/{userID}/roles", h.updateMemberRoleHandler)
			r.Post("/team/members/{membershipID}/remove", h.removeMemberHandler)
			r.Get("/projects/{projectID}/players", h.playersListPage)
			r.Get("/projects/{projectID}/players/{playerID}", h.playerDetailPage)
			r.Post("/projects/{projectID}/players/{playerID}/disable", h.playerToggleDisableHandler)
			r.Post("/projects/{projectID}/players/{playerID}/ban", h.playerToggleBanHandler)
			r.Get("/projects/{projectID}/players/invite", h.invitePlayerPage)
			r.Post("/projects/{projectID}/players/invite", h.invitePlayerHandler)
			// Leaderboard CRUD. Always-on (no feature gate); mutations are
			// gated per-handler on project:*:leaderboard, manage.
			r.Get("/projects/{projectID}/leaderboards", h.leaderboardsListPage)
			r.Get("/projects/{projectID}/leaderboards/new", h.leaderboardsNewPage)
			r.Post("/projects/{projectID}/leaderboards", h.leaderboardsCreateHandler)
			r.Get("/projects/{projectID}/leaderboards/{leaderboardID}", h.leaderboardsEditPage)
			r.Post("/projects/{projectID}/leaderboards/{leaderboardID}", h.leaderboardsUpdateHandler)
			r.Post("/projects/{projectID}/leaderboards/{leaderboardID}/delete", h.leaderboardsDeleteHandler)
			// Consolidated settings pages (writes reuse the handlers above via
			// a sanitized redirect_to).
			r.Get("/settings", h.tenantSettingsPage)
			r.Get("/projects/{projectID}/settings", h.projectSettingsPage)
			// Dedicated-server fleet surface (fleets, allocations, and the
			// matchmaker queue that feeds them). The FEATURE_FLEET_ENABLED kill
			// switch hides these routes entirely (404) when off, so operators
			// can't configure a feature the process refuses to run.
			r.Group(func(r chi.Router) {
				r.Use(h.requireFleetFeature)
				r.Get("/projects/{projectID}/matchmaker", h.matchmakerQueuePage)
				r.Get("/projects/{projectID}/matchmaker/table", h.matchmakerQueueFragment)
				r.Get("/projects/{projectID}/allocations", h.allocationsListPage)
				r.Get("/projects/{projectID}/allocations/table", h.allocationsListFragment)
				r.Get("/projects/{projectID}/allocations/new", h.allocationsNewPage)
				r.Post("/projects/{projectID}/allocations", h.allocationsAllocateHandler)
				r.Get("/projects/{projectID}/allocations/{allocID}", h.allocationsDetailPage)
				r.Get("/projects/{projectID}/allocations/{allocID}/events", h.allocationsDetailFragment)
				r.Get("/projects/{projectID}/allocations/{allocID}/deallocate", h.allocationsDeallocatePage)
				r.Post("/projects/{projectID}/allocations/{allocID}/deallocate", h.allocationsDeallocateHandler)
				r.Get("/projects/{projectID}/fleets", h.fleetsListPage)
				r.Get("/projects/{projectID}/fleets/new", h.fleetsNewPage)
				r.Get("/projects/{projectID}/fleets/new/form", h.fleetsNewFormFragment)
				r.Post("/projects/{projectID}/fleets", h.fleetsCreateHandler)
				r.Get("/projects/{projectID}/fleets/{fleetID}", h.fleetsEditPage)
				r.Post("/projects/{projectID}/fleets/{fleetID}", h.fleetsUpdateHandler)
				r.Post("/projects/{projectID}/fleets/{fleetID}/delete", h.fleetsDeleteHandler)
				r.Get("/fleet/backends", h.fleetBackendsPage)
			})
		})
		r.Route("/admin", func(r chi.Router) {
			r.Use(h.requirePlatformAdmin)
			r.Get("/team", h.platformTeamPage)
			r.Get(segTeamInvite, h.invitePlatformAdminPage)
			r.Post(segTeamInvite, h.invitePlatformAdminHandler)
			r.Get("/users", h.platformUsersPage)
			r.Post("/users/{userID}/disable", h.disableControlPanelUserHandler)
			r.Post("/users/{userID}/enable", h.enableControlPanelUserHandler)
			r.Get("/player-accounts", h.platformPlayerAccountsPage)
			r.Get("/player-accounts/{accountID}", h.platformPlayerAccountDetailPage)
			r.Post("/player-accounts/{accountID}/disable", h.disablePlayerAccountHandler)
			r.Post("/player-accounts/{accountID}/enable", h.enablePlayerAccountHandler)
			r.Get("/plugins", h.platformPluginsPage)
			r.Get("/settings", h.serverSettingsPage)
		})
		r.Post("/logout", h.logout)
	})

	// Invite acceptance is reachable without a session — the magic link
	// IS the session bootstrap. The control panel's session-bound CSRF
	// middleware doesn't apply here; instead we use the double-submit
	// cookie helper so the POST handler can still verify intent.
	r.Group(func(r chi.Router) {
		r.Use(webutil.CSRFCookie(webutil.CSRFConfig{
			Path:     pathControlPanel,
			Secure:   h.cfg.CookieSecure,
			SameSite: http.SameSiteLaxMode,
		}))
		r.Use(webutil.RequireCSRF)
		r.Get("/invite/accept", h.acceptInvitePage)
		r.Post("/invite/accept", h.acceptInviteHandler)
	})

	return r
}

func (h *Handler) assetHandler(w http.ResponseWriter, r *http.Request) {
	webassets.Serve(w, r, staticFS, "static")
}

// writePlatformAudit records a control panel-user action in platform_audit_log —
// the correct table when the actor is a control_panel_user (not a player); the
// tenant FK on audit_log would reject it. tenant_id is folded into the
// payload so the row is still correlatable to a tenant.
func (h *Handler) writePlatformAudit(ctx context.Context, tenantID, actorUserID int64, action, target string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["tenant_id"] = tenantID
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return auditlog.WritePlatform(ctx, tx, actorUserID, action, target, payload)
	})
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	tenants, err := h.listTenants(r.Context(), session.User)
	if err != nil {
		http.Error(w, "tenant list failed", http.StatusInternalServerError)
		return
	}
	webutil.Render(r, w, HomePage(HomeView{
		UserEmail:       session.User.Email,
		CSRFToken:       session.CSRFToken,
		Tenants:         tenants,
		IsPlatformAdmin: session.User.IsPlatformAdmin,
	}))

}

func (h *Handler) helpPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, HelpPage(HelpView{UserEmail: session.User.Email, CSRFToken: session.CSRFToken}))
}

func (h *Handler) newTenantPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, NewTenantPage(NewTenantView{UserEmail: session.User.Email, CSRFToken: session.CSRFToken}))
}

func (h *Handler) projectsPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projects, err := h.listProjects(r.Context(), tenantID)
	if err != nil {
		http.Error(w, msgProjectListFailed, http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	message := r.URL.Query().Get("created")
	if flash := r.URL.Query().Get("flash"); flash != "" {
		message = flash
	}
	webutil.Render(r, w, ProjectsPage(ProjectsView{
		UserEmail:    session.User.Email,
		TenantID:     tenantID,
		CSRFToken:    session.CSRFToken,
		Projects:     projects,
		Message:      message,
		FleetEnabled: h.cfg.FleetEnabled,
	}))

}

func (h *Handler) newProjectPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, NewProjectPage(NewProjectView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
	}))

}

func (h *Handler) createProjectHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	name := r.Form.Get("name")
	_, err := h.createProject(r.Context(), tenantID, name)
	if err != nil {
		session, _ := sessionFromContext(r.Context())
		view := NewProjectView{
			UserEmail: session.User.Email,
			CSRFToken: session.CSRFToken,
			TenantID:  tenantID,
			Name:      name,
			Error:     "project create failed",
		}
		status := http.StatusInternalServerError
		switch err {
		case errInvalidProjectName:
			status = http.StatusUnprocessableEntity
			view.Error = ""
			view.FieldErrors = map[string]string{"name": "Project name is required"}
		case errDuplicateProject:
			status = http.StatusConflict
			view.Error = ""
			view.FieldErrors = map[string]string{"name": "A project with that name already exists"}
		}
		w.WriteHeader(status)
		webutil.Render(r, w, NewProjectPage(view))
		return
	}
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) + "/projects?created=" + url.QueryEscape("Project \""+strings.TrimSpace(name)+"\" created.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) createTenantHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "HX-Request")
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	result, err := h.createTenant(r.Context(), signupInput{
		ActorUserID: session.User.ID,
		TenantName:  r.Form.Get("tenant_name"),
		ProjectName: r.Form.Get("project_name"),
		KeyLabel:    r.Form.Get("label"),
	})
	if err != nil {
		status := http.StatusInternalServerError
		msg := "tenant signup failed"
		if err == errInvalidSignup {
			status = http.StatusUnprocessableEntity
			msg = "Tenant name and project name are required"
		}
		w.WriteHeader(status)
		webutil.Render(r, w, FormErrorFragment(msg))
		return
	}
	webutil.Render(r, w, SignupSuccessPage(SignupSuccessView(result)))
}

func (h *Handler) openTenant(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("tenant_id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if raw == "" || err != nil || id <= 0 {
		http.Redirect(w, r, pathControlPanel, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, pathTenantsPrefix+strconv.FormatInt(id, 10)+segAPIKeys, http.StatusSeeOther)
}

func (h *Handler) apiKeys(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	keys, err := h.listAPIKeys(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "api key list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, APIKeysPage(APIKeysView{
		UserEmail: session.User.Email,
		TenantID:  tenantID,
		CSRFToken: session.CSRFToken,
		Keys:      keys,
		Message:   r.URL.Query().Get("created"),
	}))

}

func (h *Handler) newAPIKeyPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projects, err := h.listProjects(r.Context(), tenantID)
	if err != nil {
		http.Error(w, msgProjectListFailed, http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, NewAPIKeyPage(NewAPIKeyView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		Projects:  projects,
		// Default to publishable: the safer choice for someone who isn't
		// sure. A secret key embedded in a shipped client is the kind of
		// thing that ends up on a public CDN.
		KeyType: string(tenant.KeyTypePublishable),
	}))

}

func (h *Handler) createAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	rawProjectID := r.Form.Get("project_id")
	label := r.Form.Get("label")
	rawKeyType := r.Form.Get("key_type")
	keyType := tenant.KeyType(rawKeyType)
	if keyType != tenant.KeyTypePublishable && keyType != tenant.KeyTypeSecret {
		h.renderNewAPIKeyError(w, r, tenantID, label, rawProjectID, rawKeyType,
			http.StatusUnprocessableEntity,
			map[string]string{"key_type": "Pick a key type"}, "")
		return
	}
	var projectID *int64
	if rawProjectID != "" {
		id, err := strconv.ParseInt(rawProjectID, 10, 64)
		if err != nil || id <= 0 {
			h.renderNewAPIKeyError(w, r, tenantID, label, rawProjectID, rawKeyType,
				http.StatusUnprocessableEntity,
				map[string]string{"project_id": "Pick a valid project (or leave empty for tenant-wide)"}, "")
			return
		}
		projectID = &id
	}
	result, err := h.createAPIKey(r.Context(), session.User.ID, createKeyInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Label:     label,
		KeyType:   keyType,
	})
	if err != nil {
		if errors.Is(err, errProjectNotInTenant) {
			h.renderNewAPIKeyError(w, r, tenantID, label, rawProjectID, rawKeyType,
				http.StatusUnprocessableEntity,
				map[string]string{"project_id": "Pick a valid project (or leave empty for tenant-wide)"}, "")
			return
		}
		h.renderNewAPIKeyError(w, r, tenantID, label, rawProjectID, rawKeyType,
			http.StatusInternalServerError, nil, "api key create failed")
		return
	}
	webutil.Render(r, w, APIKeyCreatedPage(SignupSuccessView{
		TenantID: tenantID,
		APIKeyID: result.APIKeyID,
		APIKey:   result.APIKey,
	}, session.User.Email, session.CSRFToken))

}

// renderNewAPIKeyError re-renders the new API key page with field errors
// and the form values the user already typed.
func (h *Handler) renderNewAPIKeyError(w http.ResponseWriter, r *http.Request, tenantID int64, label, projectID, keyType string, status int, fieldErrors map[string]string, errorMsg string) {
	session, _ := sessionFromContext(r.Context())
	projects, err := h.listProjects(r.Context(), tenantID)
	if err != nil {
		http.Error(w, msgProjectListFailed, http.StatusInternalServerError)
		return
	}
	if keyType == "" {
		keyType = string(tenant.KeyTypePublishable)
	}
	w.WriteHeader(status)
	webutil.Render(r, w, NewAPIKeyPage(NewAPIKeyView{
		UserEmail:   session.User.Email,
		CSRFToken:   session.CSRFToken,
		TenantID:    tenantID,
		Projects:    projects,
		Label:       label,
		ProjectID:   projectID,
		KeyType:     keyType,
		Error:       errorMsg,
		FieldErrors: fieldErrors,
	}))

}

func (h *Handler) updateAPIKeyLabelHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, apiKeyID, ok := parseTenantAndAPIKeyIDs(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	if err := h.updateAPIKeyLabel(r.Context(), session.User.ID, tenantID, apiKeyID, r.Form.Get("label")); err != nil {
		http.Error(w, "api key label failed", http.StatusInternalServerError)
		return
	}
	htmxRedirect(w, r, pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+segAPIKeys)
}

func (h *Handler) updateAPIKeyScopesHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, apiKeyID, ok := parseTenantAndAPIKeyIDs(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	grant := r.Form.Get("action") == "grant"
	scope := r.Form.Get("scope")
	session, _ := sessionFromContext(r.Context())
	switch err := h.setAPIKeyScope(r.Context(), session.User.ID, tenantID, apiKeyID, scope, grant); {
	case err == nil:
		htmxRedirect(w, r, pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+segAPIKeys)
	case errors.Is(err, errInvalidScope), errors.Is(err, errScopeNotGrantable):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, errKeyNotInTenant):
		http.Error(w, "api key not found", http.StatusNotFound)
	default:
		http.Error(w, "api key scope update failed", http.StatusInternalServerError)
	}
}

func (h *Handler) revokeAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, apiKeyID, ok := parseTenantAndAPIKeyIDs(w, r)
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	if err := h.revokeAPIKey(r.Context(), session.User.ID, tenantID, apiKeyID); err != nil {
		http.Error(w, "api key revoke failed", http.StatusInternalServerError)
		return
	}
	htmxRedirect(w, r, pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+segAPIKeys)
}

func (h *Handler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := h.sessionFromRequest(r)
		if !ok {
			http.Redirect(w, r, pathControlPanelLogin, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(contextWithSession(r.Context(), session)))
	})
}

func (h *Handler) requirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := sessionFromContext(r.Context())
		if !ok {
			http.Error(w, "missing session", http.StatusUnauthorized)
			return
		}
		if !session.User.IsPlatformAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireTenantAccess(minRole string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tenantID, ok := parsePathID(w, r, "tenantID")
			if !ok {
				return
			}
			session, ok := sessionFromContext(r.Context())
			if !ok {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			if h.rbac == nil {
				http.Error(w, "authorization unavailable", http.StatusInternalServerError)
				return
			}
			obj, act := tenantAccessPermission(minRole)
			allowed, err := h.rbac.CanControlPanel(rbac.ControlPanelUser{
				ID:              session.User.ID,
				IsPlatformAdmin: session.User.IsPlatformAdmin,
			}, tenantID, obj, act)
			if err != nil {
				http.Error(w, "tenant access check failed", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func tenantAccessPermission(minRole string) (string, string) {
	switch minRole {
	case roleOwner:
		return rbac.ObjectTenant, rbac.ActionManage
	case roleAdmin:
		return rbac.ObjectProject, rbac.ActionManage
	default:
		return rbac.ObjectProject, rbac.ActionRead
	}
}

// htmxRedirect sends an HX-Redirect header for HTMX clients (which would
// otherwise transparently follow a 303 into the swap target) and a plain
// 303 See Other for everything else.
func htmxRedirect(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func parseTenantAndAPIKeyIDs(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return 0, 0, false
	}
	apiKeyID, ok := parsePathID(w, r, "apiKeyID")
	if !ok {
		return 0, 0, false
	}
	return tenantID, apiKeyID, true
}

func parsePathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// clientIP extracts the audit IP, delegating to the shared ProxyTrust resolver
// so audit rows and the per-IP limiter agree on the real client. Forwarded
// headers are honored only when the TCP peer is a configured trusted proxy.
func (h *Handler) clientIP(r *http.Request) string {
	return h.proxyTrust.ClientIP(r)
}
