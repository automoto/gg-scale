package dashboard

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/webutil"
)

// fleetsListPage renders the project's fleet templates. An operator creates
// at least one template before allocations succeed; the page links to /new
// and shows the running ggscale's backend so mismatches are obvious.
func (h *Handler) fleetsListPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	view := FleetsListView{
		UserEmail:         session.User.Email,
		CSRFToken:         session.CSRFToken,
		TenantID:          tenantID,
		ProjectID:         projectID,
		BackendConfigured: h.fleetBackendName(),
		Enabled:           h.fleetEnabled(),
		Message:           r.URL.Query().Get("flash"),
	}
	if h.fleetEnabled() {
		tenantCtx := db.WithTenant(r.Context(), tenantID)
		fleets, err := h.fleet.Fleets().ListForProject(tenantCtx, projectID)
		if err != nil {
			slog.ErrorContext(r.Context(), "fleets list failed", "err", err)
			http.Error(w, "fleets list failed", http.StatusInternalServerError)
			return
		}
		for _, f := range fleets {
			view.Fleets = append(view.Fleets, FleetRowView{
				ID:             f.ID,
				Name:           f.Name,
				Backend:        f.Backend,
				BackendMatches: f.Backend == view.BackendConfigured,
				Summary:        summarizeFleetConfig(f.Backend, f.Config),
			})
		}
	}
	webutil.Render(r, w, FleetsListPage(view))
}

// fleetsNewPage renders the empty create form. The backend dropdown defaults
// to the running ggscale's backend; switching it swaps the per-backend
// field set via fleetsNewFormFragment (HTMX hx-get).
func (h *Handler) fleetsNewPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	view := NewFleetView{
		UserEmail:         session.User.Email,
		CSRFToken:         session.CSRFToken,
		TenantID:          tenantID,
		ProjectID:         projectID,
		BackendConfigured: h.fleetBackendName(),
		Backend:           h.fleetBackendName(),
	}
	webutil.Render(r, w, NewFleetPage(view))
}

// fleetsNewFormFragment is the HTMX partial that swaps the per-backend
// field block when the backend selector changes on the create page. Keeps
// the create form on a single page (per the chosen "single page, fields
// swap" UX).
func (h *Handler) fleetsNewFormFragment(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	backend := strings.TrimSpace(r.URL.Query().Get("backend"))
	if backend == "" {
		backend = h.fleetBackendName()
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, FleetBackendFieldsFragment(NewFleetView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
		Backend:   backend,
	}))
}

// fleetsCreateHandler validates the form and inserts a fleet row. On
// validation failure the form re-renders with field errors so the operator
// doesn't lose what they typed.
func (h *Handler) fleetsCreateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	if h.fleet == nil {
		http.Error(w, "no fleet backend configured", http.StatusServiceUnavailable)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	name := strings.TrimSpace(r.Form.Get("name"))
	backend := strings.TrimSpace(r.Form.Get("backend"))
	cfg, fieldErrors := parseFleetConfigForm(backend, r.Form)
	if name == "" {
		fieldErrors["name"] = "Name is required."
	}
	if backend == "" {
		fieldErrors["backend"] = "Backend is required."
	}
	view := NewFleetView{
		UserEmail:         session.User.Email,
		CSRFToken:         session.CSRFToken,
		TenantID:          tenantID,
		ProjectID:         projectID,
		BackendConfigured: h.fleetBackendName(),
		Name:              name,
		Backend:           backend,
		Config:            cfg,
		FieldErrors:       fieldErrors,
	}
	if len(fieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, NewFleetPage(view))
		return
	}

	tenantCtx := db.WithTenant(r.Context(), tenantID)
	f, err := h.fleet.Fleets().Create(tenantCtx, fleet.FleetCreate{
		ProjectID: projectID,
		Name:      name,
		Backend:   backend,
		Config:    cfg,
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "create fleet failed", "err", err)
		view.Error = "Create failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		webutil.Render(r, w, NewFleetPage(view))
		return
	}
	if auditErr := h.writeFleetAudit(r.Context(), tenantID, session.User.ID, "fleet.template.create", strconv.FormatInt(f.ID, 10), map[string]any{
		"project_id": projectID,
		"fleet_name": f.Name,
		"backend":    f.Backend,
	}); auditErr != nil {
		slog.WarnContext(r.Context(), "audit log: fleet.template.create", "err", auditErr)
	}
	target := fleetsBasePath(tenantID, projectID) + "?flash=" + url.QueryEscape("Fleet \""+f.Name+"\" created.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// fleetsEditPage renders the edit form prefilled with the fleet's current
// values. Name can change; backend is fixed (changing backend would break
// existing allocations referencing the row).
func (h *Handler) fleetsEditPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	fleetID, ok := parsePathID(w, r, "fleetID")
	if !ok {
		return
	}
	if h.fleet == nil {
		http.Error(w, "no fleet backend configured", http.StatusServiceUnavailable)
		return
	}
	tenantCtx := db.WithTenant(r.Context(), tenantID)
	f, err := h.fleet.Fleets().GetByID(tenantCtx, fleetID)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "fleet lookup failed", http.StatusInternalServerError)
		return
	}
	if f.ProjectID != projectID {
		http.NotFound(w, r)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, EditFleetPage(EditFleetView{
		UserEmail:         session.User.Email,
		CSRFToken:         session.CSRFToken,
		TenantID:          tenantID,
		ProjectID:         projectID,
		FleetID:           f.ID,
		Name:              f.Name,
		Backend:           f.Backend,
		Config:            f.Config,
		BackendConfigured: h.fleetBackendName(),
	}))
}

// fleetsUpdateHandler persists changes to name / config. Backend is fixed
// on update; recreating under a different backend is the supported path.
func (h *Handler) fleetsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	fleetID, ok := parsePathID(w, r, "fleetID")
	if !ok {
		return
	}
	if h.fleet == nil {
		http.Error(w, "no fleet backend configured", http.StatusServiceUnavailable)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	tenantCtx := db.WithTenant(r.Context(), tenantID)
	existing, err := h.fleet.Fleets().GetByID(tenantCtx, fleetID)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "fleet lookup failed", http.StatusInternalServerError)
		return
	}
	if existing.ProjectID != projectID {
		http.NotFound(w, r)
		return
	}
	session, _ := sessionFromContext(r.Context())
	name := strings.TrimSpace(r.Form.Get("name"))
	cfg, fieldErrors := parseFleetConfigForm(existing.Backend, r.Form)
	if name == "" {
		fieldErrors["name"] = "Name is required."
	}
	view := EditFleetView{
		UserEmail:         session.User.Email,
		CSRFToken:         session.CSRFToken,
		TenantID:          tenantID,
		ProjectID:         projectID,
		FleetID:           fleetID,
		Name:              name,
		Backend:           existing.Backend,
		Config:            cfg,
		BackendConfigured: h.fleetBackendName(),
		FieldErrors:       fieldErrors,
	}
	if len(fieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, EditFleetPage(view))
		return
	}
	if err := h.fleet.Fleets().Update(tenantCtx, fleet.FleetUpdate{
		ID:      fleetID,
		Name:    name,
		Backend: existing.Backend,
		Config:  cfg,
	}); err != nil {
		slog.ErrorContext(r.Context(), "update fleet failed", "err", err)
		view.Error = "Update failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		webutil.Render(r, w, EditFleetPage(view))
		return
	}
	if auditErr := h.writeFleetAudit(r.Context(), tenantID, session.User.ID, "fleet.template.update", strconv.FormatInt(fleetID, 10), map[string]any{
		"project_id": projectID,
		"fleet_name": name,
	}); auditErr != nil {
		slog.WarnContext(r.Context(), "audit log: fleet.template.update", "err", auditErr)
	}
	target := fleetsBasePath(tenantID, projectID) + "?flash=" + url.QueryEscape("Fleet \""+name+"\" updated.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// fleetsDeleteHandler soft-deletes the template. Historical allocations
// keep their fleet_id reference; the dashboard surfaces the (now-deleted)
// name from the row.
func (h *Handler) fleetsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	fleetID, ok := parsePathID(w, r, "fleetID")
	if !ok {
		return
	}
	if h.fleet == nil {
		http.Error(w, "no fleet backend configured", http.StatusServiceUnavailable)
		return
	}
	tenantCtx := db.WithTenant(r.Context(), tenantID)
	if err := h.fleet.Fleets().SoftDelete(tenantCtx, fleetID); err != nil {
		slog.ErrorContext(r.Context(), "delete fleet failed", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	if auditErr := h.writeFleetAudit(r.Context(), tenantID, session.User.ID, "fleet.template.delete", strconv.FormatInt(fleetID, 10), map[string]any{
		"project_id": projectID,
	}); auditErr != nil {
		slog.WarnContext(r.Context(), "audit log: fleet.template.delete", "err", auditErr)
	}
	target := fleetsBasePath(tenantID, projectID) + "?flash=" + url.QueryEscape("Fleet deleted.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// parseFleetConfigForm extracts the per-backend fields from the submitted
// form into a flat string map matching what backends consume on Allocate.
// Returns field-level errors for required-field violations.
func parseFleetConfigForm(backend string, form url.Values) (map[string]string, map[string]string) {
	cfg := map[string]string{}
	errs := map[string]string{}
	switch backend {
	case "docker":
		cfg["image"] = strings.TrimSpace(form.Get("image"))
		cfg["port"] = strings.TrimSpace(form.Get("port"))
		cfg["probe_type"] = strings.TrimSpace(form.Get("probe_type"))
		cfg["probe_path"] = strings.TrimSpace(form.Get("probe_path"))
		if form.Get("pull_image") == "on" || form.Get("pull_image") == "true" {
			cfg["pull_image"] = "true"
		}
		if cfg["image"] == "" {
			errs["image"] = "Image is required."
		}
		if cfg["port"] == "" {
			errs["port"] = "Port is required."
		} else if n, err := strconv.Atoi(cfg["port"]); err != nil || n <= 0 || n > 65535 {
			errs["port"] = "Port must be a number between 1 and 65535."
		}
	case "agones":
		cfg["fleet_name"] = strings.TrimSpace(form.Get("fleet_name"))
		if ns := strings.TrimSpace(form.Get("namespace")); ns != "" {
			cfg["namespace"] = ns
		}
		keys := form["selector_key[]"]
		if len(keys) > fleetFormKeyValuePairsCap {
			errs["selector_key"] = "Too many selector keys."
			keys = keys[:fleetFormKeyValuePairsCap]
		}
		for i, k := range keys {
			k = strings.TrimSpace(k)
			if k == "" || len(k) > fleetFormKeyOrValueLenCap || i >= len(form["selector_value[]"]) {
				continue
			}
			v := strings.TrimSpace(form["selector_value[]"][i])
			if len(v) > fleetFormKeyOrValueLenCap {
				continue
			}
			cfg["selector."+k] = v
		}
		if cfg["fleet_name"] == "" {
			errs["fleet_name"] = "Fleet name is required."
		}
	default:
		// plugin:<name> — free-form key/value pairs.
		keys := form["config_key[]"]
		if len(keys) > fleetFormKeyValuePairsCap {
			errs["config_key"] = "Too many config keys."
			keys = keys[:fleetFormKeyValuePairsCap]
		}
		for i, k := range keys {
			k = strings.TrimSpace(k)
			if k == "" || len(k) > fleetFormKeyOrValueLenCap || i >= len(form["config_value[]"]) {
				continue
			}
			v := strings.TrimSpace(form["config_value[]"][i])
			if len(v) > fleetFormKeyOrValueLenCap {
				continue
			}
			cfg[k] = v
		}
	}
	return cfg, errs
}

// fleetFormKeyValuePairsCap is the upper bound on selector_key[] / config_key[]
// arrays. Bounded only by the overall form-body limit before this; a single
// request used to be able to stuff thousands of entries into a fleet config,
// JSON-serialise them, store them, and re-render them on the next page load.
const fleetFormKeyValuePairsCap = 64

// fleetFormKeyOrValueLenCap bounds a single key or value within those forms.
const fleetFormKeyOrValueLenCap = 256

// summarizeFleetConfig returns a one-line preview for the list page, so
// operators can scan templates without opening each one.
func summarizeFleetConfig(backend string, cfg map[string]string) string {
	switch backend {
	case "docker":
		return cfg["image"] + " :" + cfg["port"]
	case "agones":
		return cfg["fleet_name"]
	default:
		// plugin: render the first few keys joined.
		var keys []string
		for k := range cfg {
			keys = append(keys, k+"="+cfg[k])
		}
		return strings.Join(keys, " ")
	}
}
