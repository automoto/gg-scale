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
		http.Error(w, msgNoFleetBackend, http.StatusServiceUnavailable)
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
	if !h.requireDashboardFleetMutation(w, r, tenantID, projectID, backend) {
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
	target := fleetsBasePath(tenantID, projectID) + queryFlash + url.QueryEscape("Fleet \""+f.Name+"\" created.")
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
		http.Error(w, msgNoFleetBackend, http.StatusServiceUnavailable)
		return
	}
	tenantCtx := db.WithTenant(r.Context(), tenantID)
	f, err := h.fleet.Fleets().GetByID(tenantCtx, fleetID)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, msgFleetLookupFailed, http.StatusInternalServerError)
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
		http.Error(w, msgNoFleetBackend, http.StatusServiceUnavailable)
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
		http.Error(w, msgFleetLookupFailed, http.StatusInternalServerError)
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
	if !h.requireDashboardFleetMutation(w, r, tenantID, projectID, existing.Backend) {
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
	target := fleetsBasePath(tenantID, projectID) + queryFlash + url.QueryEscape("Fleet \""+name+"\" updated.")
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
		http.Error(w, msgNoFleetBackend, http.StatusServiceUnavailable)
		return
	}
	tenantCtx := db.WithTenant(r.Context(), tenantID)
	existing, err := h.fleet.Fleets().GetByID(tenantCtx, fleetID)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, msgFleetLookupFailed, http.StatusInternalServerError)
		return
	}
	if existing.ProjectID != projectID {
		http.NotFound(w, r)
		return
	}
	if !h.requireDashboardFleetMutation(w, r, tenantID, projectID, existing.Backend) {
		return
	}
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
	target := fleetsBasePath(tenantID, projectID) + queryFlash + url.QueryEscape("Fleet deleted.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// parseFleetConfigForm extracts the per-backend fields from the submitted
// form into a flat string map matching what backends consume on Allocate.
// Returns field-level errors for required-field violations.
func parseFleetConfigForm(backend string, form url.Values) (map[string]string, map[string]string) {
	switch backend {
	case "docker":
		return parseDockerFleetConfig(form)
	case "agones":
		return parseAgonesFleetConfig(form)
	default:
		// plugin:<name> — free-form key/value pairs.
		return parsePluginFleetConfig(form)
	}
}

func parseDockerFleetConfig(form url.Values) (map[string]string, map[string]string) {
	cfg := map[string]string{
		"image":      strings.TrimSpace(form.Get("image")),
		"port":       strings.TrimSpace(form.Get("port")),
		"probe_type": strings.TrimSpace(form.Get("probe_type")),
		"probe_path": strings.TrimSpace(form.Get("probe_path")),
	}
	if form.Get("pull_image") == "on" || form.Get("pull_image") == "true" {
		cfg["pull_image"] = "true"
	}
	errs := map[string]string{}
	if cfg["image"] == "" {
		errs["image"] = "Image is required."
	}
	switch n, err := strconv.Atoi(cfg["port"]); {
	case cfg["port"] == "":
		errs["port"] = "Port is required."
	case err != nil || n <= 0 || n > 65535:
		errs["port"] = "Port must be a number between 1 and 65535."
	}
	return cfg, errs
}

func parseAgonesFleetConfig(form url.Values) (map[string]string, map[string]string) {
	cfg := map[string]string{"fleet_name": strings.TrimSpace(form.Get("fleet_name"))}
	if ns := strings.TrimSpace(form.Get("namespace")); ns != "" {
		cfg["namespace"] = ns
	}
	errs := map[string]string{}
	pairs, overCap := boundedKeyValuePairs(form["selector_key[]"], form["selector_value[]"])
	if overCap {
		errs["selector_key"] = "Too many selector keys."
	}
	for k, v := range pairs {
		cfg["selector."+k] = v
	}
	if cfg["fleet_name"] == "" {
		errs["fleet_name"] = "Fleet name is required."
	}
	return cfg, errs
}

func parsePluginFleetConfig(form url.Values) (map[string]string, map[string]string) {
	cfg, overCap := boundedKeyValuePairs(form["config_key[]"], form["config_value[]"])
	errs := map[string]string{}
	if overCap {
		errs["config_key"] = "Too many config keys."
	}
	return cfg, errs
}

// boundedKeyValuePairs zips a key[] / value[] form pair into a map, enforcing
// the per-form count and per-entry length caps. Blank keys and entries past
// the value slice are skipped. overCap reports whether the key count was
// truncated so callers can surface a field error.
func boundedKeyValuePairs(keys, values []string) (pairs map[string]string, overCap bool) {
	pairs = map[string]string{}
	if len(keys) > fleetFormKeyValuePairsCap {
		overCap = true
		keys = keys[:fleetFormKeyValuePairsCap]
	}
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || len(k) > fleetFormKeyOrValueLenCap || i >= len(values) {
			continue
		}
		v := strings.TrimSpace(values[i])
		if len(v) > fleetFormKeyOrValueLenCap {
			continue
		}
		pairs[k] = v
	}
	return pairs, overCap
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
