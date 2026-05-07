package dashboard

import (
	"embed"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/ratelimit"
)

const maxFormBodyBytes = 1 << 20

//go:embed static
var staticFS embed.FS

// Handler owns dashboard HTTP routes.
type Handler struct {
	pool      *db.Pool
	cache     cache.Store
	limiter   ratelimit.Limiter
	reg       prometheus.Registerer
	cfg       Config
	bootstrap *Bootstrap
	now       func() time.Time
}

// New builds the dashboard router. Callers should only mount it when
// Config.Enabled returns true.
func New(pool *db.Pool, store cache.Store, limiter ratelimit.Limiter, reg prometheus.Registerer, cfg Config, bootstrap *Bootstrap) http.Handler {
	if bootstrap == nil {
		bootstrap = DisabledBootstrap()
	}
	h := &Handler{
		pool:      pool,
		cache:     store,
		limiter:   limiter,
		reg:       reg,
		cfg:       cfg,
		bootstrap: bootstrap,
		now:       time.Now,
	}

	r := chi.NewRouter()
	r.Use(securityHeaders)
	r.Get("/assets/*", h.assetHandler)
	r.Group(func(r chi.Router) {
		if limiter != nil {
			r.Use(ratelimit.NewIPLimiter(limiter, ratelimit.AuthIPRate, ratelimit.AuthIPBurst, reg))
		}
		r.Get("/setup", h.setup)
		r.Post("/setup", h.completeSetup)
		r.Get("/login", h.loginPage)
		r.Post("/login", h.login)
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
		r.Route("/tenants/{tenantID}", func(r chi.Router) {
			r.Use(h.requireTenantAccess(roleAdmin))
			r.Get("/projects", h.projectsPage)
			r.Post("/projects", h.createProjectHandler)
			r.Get("/api-keys", h.apiKeys)
			r.Post("/api-keys", h.createAPIKeyHandler)
			r.Post("/api-keys/{apiKeyID}/label", h.updateAPIKeyLabelHandler)
			r.Post("/api-keys/{apiKeyID}/revoke", h.revokeAPIKeyHandler)
		})
		r.Post("/logout", h.logout)
	})

	return r
}

func (h *Handler) assetHandler(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "*")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFileFS(w, r, staticFS, "static/"+name)
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	tenants, err := h.listTenants(r.Context(), session.User)
	if err != nil {
		http.Error(w, "tenant list failed", http.StatusInternalServerError)
		return
	}
	render(r, w, HomePage(HomeView{UserEmail: session.User.Email, CSRFToken: session.CSRFToken, Tenants: tenants}))
}

func (h *Handler) helpPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	render(r, w, HelpPage(HelpView{UserEmail: session.User.Email, CSRFToken: session.CSRFToken}))
}

func (h *Handler) newTenantPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	render(r, w, NewTenantPage(NewTenantView{UserEmail: session.User.Email, CSRFToken: session.CSRFToken}))
}

func (h *Handler) projectsPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projects, err := h.listProjects(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "project list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	render(r, w, ProjectsPage(ProjectsView{
		UserEmail: session.User.Email,
		TenantID:  tenantID,
		CSRFToken: session.CSRFToken,
		Projects:  projects,
	}))
}

func (h *Handler) createProjectHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	_, err := h.createProject(r.Context(), tenantID, r.Form.Get("name"))
	if err != nil {
		session, _ := sessionFromContext(r.Context())
		projects, listErr := h.listProjects(r.Context(), tenantID)
		if listErr != nil {
			http.Error(w, "project list failed", http.StatusInternalServerError)
			return
		}
		status := http.StatusInternalServerError
		view := ProjectsView{
			UserEmail: session.User.Email,
			TenantID:  tenantID,
			CSRFToken: session.CSRFToken,
			Projects:  projects,
			Error:     "project create failed",
		}
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
		render(r, w, ProjectsPage(view))
		return
	}
	htmxRedirect(w, r, "/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/projects")
}

func (h *Handler) createTenantHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "HX-Request")
	if !parseForm(w, r) {
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
		render(r, w, FormErrorFragment(msg))
		return
	}
	render(r, w, SignupSuccessPage(SignupSuccessView(result)))
}

func (h *Handler) openTenant(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("tenant_id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if raw == "" || err != nil || id <= 0 {
		http.Redirect(w, r, "/v1/dashboard", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/v1/dashboard/tenants/"+strconv.FormatInt(id, 10)+"/api-keys", http.StatusSeeOther)
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
	projects, err := h.listProjects(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "project list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	render(r, w, APIKeysPage(APIKeysView{
		UserEmail: session.User.Email,
		TenantID:  tenantID,
		CSRFToken: session.CSRFToken,
		Keys:      keys,
		Projects:  projects,
	}))
}

func (h *Handler) createAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "HX-Request")
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	var projectID *int64
	if raw := r.Form.Get("project_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			render(r, w, FormErrorFragment("Pick a valid project (or leave empty for tenant-wide)"))
			return
		}
		projectID = &id
	}
	result, err := h.createAPIKey(r.Context(), createKeyInput{
		TenantID:  tenantID,
		ProjectID: projectID,
		Label:     r.Form.Get("label"),
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		render(r, w, FormErrorFragment("api key create failed"))
		return
	}
	render(r, w, APIKeyCreatedPage(SignupSuccessView{
		TenantID: tenantID,
		APIKeyID: result.APIKeyID,
		APIKey:   result.APIKey,
	}))
}

func (h *Handler) updateAPIKeyLabelHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, apiKeyID, ok := parseTenantAndAPIKeyIDs(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	if err := h.updateAPIKeyLabel(r.Context(), tenantID, apiKeyID, r.Form.Get("label")); err != nil {
		http.Error(w, "api key label failed", http.StatusInternalServerError)
		return
	}
	htmxRedirect(w, r, "/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys")
}

func (h *Handler) revokeAPIKeyHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, apiKeyID, ok := parseTenantAndAPIKeyIDs(w, r)
	if !ok {
		return
	}
	if !parseForm(w, r) {
		return
	}
	if err := h.revokeAPIKey(r.Context(), tenantID, apiKeyID); err != nil {
		http.Error(w, "api key revoke failed", http.StatusInternalServerError)
		return
	}
	htmxRedirect(w, r, "/v1/dashboard/tenants/"+strconv.FormatInt(tenantID, 10)+"/api-keys")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := h.sessionFromRequest(r)
		if !ok {
			http.Redirect(w, r, "/v1/dashboard/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(contextWithSession(r.Context(), session)))
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
			allowed, err := h.userCanAccessTenant(r.Context(), session.User, tenantID, minRole)
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

func render(r *http.Request, w http.ResponseWriter, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "dashboard template render failed", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
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

func parseForm(w http.ResponseWriter, r *http.Request) bool {
	if r.Form != nil {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBodyBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return false
	}
	return true
}

// clientIP extracts the real client IP for audit purposes.
// CF-Connecting-IP and X-Real-IP are trusted only when a proxy strips them on
// ingress (see ARCHITECTURE.md § "Reverse-proxy IP trust"). Falls back to
// RemoteAddr when neither header is present.
func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
