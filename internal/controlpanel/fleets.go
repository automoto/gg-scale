package controlpanel

import (
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/fleet"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/webutil"
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
		deleted, err := h.fleet.Fleets().ListDeletedForProject(tenantCtx, projectID)
		if err != nil {
			slog.ErrorContext(r.Context(), "deleted fleets list failed", "err", err)
			http.Error(w, "fleets list failed", http.StatusInternalServerError)
			return
		}
		for _, f := range deleted {
			view.DeletedFleets = append(view.DeletedFleets, FleetRowView{ID: f.ID, Name: f.Name, Backend: f.Backend})
		}
		for _, f := range fleets {
			view.Fleets = append(view.Fleets, FleetRowView{
				ID:             f.ID,
				Name:           f.Name,
				Backend:        f.Backend,
				BackendMatches: fleet.BackendMatches(f.Backend, view.BackendConfigured),
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
		Config:            fleetFormConfig(backend, cfg),
		FieldErrors:       fieldErrors,
	}
	if len(fieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, NewFleetPage(view))
		return
	}
	if !h.requireControlPanelFleetMutation(w, r, tenantID, projectID, backend) {
		return
	}

	f, err := fleet.CreateFleet(r.Context(), h.pool, h.credentialCipher, tenantID, fleet.FleetCreate{
		ProjectID: projectID,
		Name:      name,
		Backend:   backend,
		Config:    cfg,
	}, projectadmin.Actor{UserID: session.User.ID})
	if err != nil {
		var status int
		view.Error, status = fleetFormError("Create failed", err)
		slog.ErrorContext(r.Context(), "create fleet failed", "err", err)
		w.WriteHeader(status)
		webutil.Render(r, w, NewFleetPage(view))
		return
	}
	redirectFlash(w, r, fleetsBasePath(tenantID, projectID), "Fleet \""+f.Name+"\" created.")
}

// fleetsEditPage renders the edit form prefilled with the fleet's current
// values. Name can change; backend is fixed (changing backend would break
// existing allocations referencing the row).
func (h *Handler) fleetsEditPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, f, ok := h.projectFleet(w, r, false)
	if !ok {
		return
	}
	if f.DeletedAt != nil {
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
		Config:            fleetFormConfig(f.Backend, f.Config),
		BackendConfigured: h.fleetBackendName(),
	}))
}

// fleetsUpdateHandler persists changes to name / config. Backend is fixed
// on update; recreating under a different backend is the supported path.
func (h *Handler) fleetsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, existing, ok := h.projectFleet(w, r, true)
	if !ok {
		return
	}
	fleetID := existing.ID
	session, _ := sessionFromContext(r.Context())
	name := strings.TrimSpace(r.Form.Get("name"))
	cfg, fieldErrors := parseFleetConfigForm(existing.Backend, r.Form)
	if fleet.ConfigIsSecret(existing.Backend) {
		keepStoredPluginValues(cfg, existing.Config)
	}
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
		Config:            fleetFormConfig(existing.Backend, cfg),
		BackendConfigured: h.fleetBackendName(),
		FieldErrors:       fieldErrors,
	}
	if len(fieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, EditFleetPage(view))
		return
	}
	if !h.requireControlPanelFleetMutation(w, r, tenantID, projectID, existing.Backend) {
		return
	}
	if _, err := fleet.UpdateFleet(r.Context(), h.pool, h.credentialCipher, tenantID, projectID, fleetID, &name, cfg, projectadmin.Actor{UserID: session.User.ID}); err != nil {
		if errors.Is(err, fleet.ErrFleetNotFound) {
			http.NotFound(w, r)
			return
		}
		var status int
		view.Error, status = fleetFormError("Update failed", err)
		slog.ErrorContext(r.Context(), "update fleet failed", "err", err)
		w.WriteHeader(status)
		webutil.Render(r, w, EditFleetPage(view))
		return
	}
	redirectFlash(w, r, fleetsBasePath(tenantID, projectID), "Fleet \""+name+"\" updated.")
}

// fleetsDeleteHandler soft-deletes the template. Historical allocations
// keep their fleet_id reference; the control panel surfaces the (now-deleted)
// name from the row.
func (h *Handler) fleetsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, existing, ok := h.projectFleet(w, r, false)
	if !ok {
		return
	}
	if !h.requireControlPanelFleetMutation(w, r, tenantID, projectID, existing.Backend) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	if err := fleet.DeleteFleet(r.Context(), h.pool, tenantID, projectID, existing.ID, projectadmin.Actor{UserID: session.User.ID}); err != nil {
		if errors.Is(err, fleet.ErrFleetNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.ErrorContext(r.Context(), "delete fleet failed", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	redirectFlash(w, r, fleetsBasePath(tenantID, projectID), "Fleet deleted.")
}

// fleetsRestoreHandler brings back a deleted template. A live template with
// the same name blocks the restore.
func (h *Handler) fleetsRestoreHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, existing, ok := h.projectFleet(w, r, false)
	if !ok {
		return
	}
	if !h.requireControlPanelFleetMutation(w, r, tenantID, projectID, existing.Backend) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	f, err := fleet.RestoreFleet(r.Context(), h.pool, h.credentialCipher, tenantID, projectID, existing.ID, projectadmin.Actor{UserID: session.User.ID})
	base := fleetsBasePath(tenantID, projectID)
	switch {
	case errors.Is(err, fleet.ErrFleetNotFound):
		http.NotFound(w, r)
	case errors.Is(err, fleet.ErrFleetNameTaken):
		redirectFlash(w, r, base, "Cannot restore: a fleet with that name exists. Rename or delete it first.")
	case err != nil:
		slog.ErrorContext(r.Context(), "restore fleet failed", "err", err)
		http.Error(w, "restore failed", http.StatusInternalServerError)
	default:
		redirectFlash(w, r, base, "Fleet \""+f.Name+"\" restored.")
	}
}

// projectFleet parses the IDs of a fleet route and returns the fleet (live or
// deleted) of the project. On failure it writes the answer and ok is false.
// parseForm reads the form after the backend check and before the lookup.
func (h *Handler) projectFleet(w http.ResponseWriter, r *http.Request, parseForm bool) (tenantID, projectID int64, f *fleet.Fleet, ok bool) {
	tenantID, projectID, ok = h.parseTenantAndProject(w, r)
	if !ok {
		return 0, 0, nil, false
	}
	fleetID, ok := parsePathID(w, r, "fleetID")
	if !ok {
		return 0, 0, nil, false
	}
	if h.fleet == nil {
		http.Error(w, msgNoFleetBackend, http.StatusServiceUnavailable)
		return 0, 0, nil, false
	}
	if parseForm && !webutil.ParseForm(w, r) {
		return 0, 0, nil, false
	}
	f, err := h.fleet.Fleets().GetByID(db.WithTenant(r.Context(), tenantID), fleetID)
	if errors.Is(err, fleet.ErrFleetNotFound) {
		http.NotFound(w, r)
		return 0, 0, nil, false
	}
	if err != nil {
		http.Error(w, msgFleetLookupFailed, http.StatusInternalServerError)
		return 0, 0, nil, false
	}
	if f.ProjectID != projectID {
		http.NotFound(w, r)
		return 0, 0, nil, false
	}
	return tenantID, projectID, f, true
}

// redirectFlash redirects to base with a flash message.
func redirectFlash(w http.ResponseWriter, r *http.Request, base, msg string) {
	http.Redirect(w, r, base+queryFlash+url.QueryEscape(msg), http.StatusSeeOther) //nolint:gosec // base is built from parsed integer IDs and msg is escaped
}

// fleetFormError returns the text and HTTP status for a failed fleet write.
// An invalid input or a taken name shows its own text; other errors show a
// fixed text, so no database text reaches the page.
func fleetFormError(prefix string, err error) (string, int) {
	var invalid fleet.InvalidError
	switch {
	case errors.As(err, &invalid):
		return prefix + ": " + invalid.Error(), http.StatusUnprocessableEntity
	case errors.Is(err, fleet.ErrFleetNameTaken):
		return prefix + ": a fleet with that name already exists.", http.StatusConflict
	}
	return prefix + ".", http.StatusInternalServerError
}

// parseFleetConfigForm extracts the per-backend fields from the submitted
// form into a flat string map matching what backends consume on Allocate.
// Returns field-level errors for required-field violations.
func parseFleetConfigForm(backend string, form url.Values) (map[string]string, map[string]string) {
	switch backend {
	case "agones":
		return parseAgonesFleetConfig(form)
	default:
		// plugin:<name> — free-form key/value pairs.
		return parsePluginFleetConfig(form)
	}
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
	if err := fleet.ValidateConfig("agones", cfg); err != nil {
		errs["fleet_name"] = err.Error()
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
	if len(keys) > fleet.MaxConfigKeys {
		overCap = true
		keys = keys[:fleet.MaxConfigKeys]
	}
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || len(k) > fleet.MaxConfigKeyOrVal || i >= len(values) {
			continue
		}
		v := strings.TrimSpace(values[i])
		if len(v) > fleet.MaxConfigKeyOrVal {
			continue
		}
		pairs[k] = v
	}
	return pairs, overCap
}

// summarizeFleetConfig returns a one-line preview for the list page, so
// operators can scan templates without opening each one. A plugin config
// shows its keys only, because a value can be a secret.
func summarizeFleetConfig(backend string, cfg map[string]string) string {
	if !fleet.ConfigIsSecret(backend) {
		return cfg["fleet_name"]
	}
	return strings.Join(slices.Sorted(maps.Keys(cfg)), " ")
}

// fleetFormConfig returns the config that a fleet form shows: an agones
// config as it is, and the keys of a plugin config with empty values.
func fleetFormConfig(backend string, cfg map[string]string) map[string]string {
	if !fleet.ConfigIsSecret(backend) {
		return cfg
	}
	out := make(map[string]string, len(cfg))
	for k := range cfg {
		out[k] = ""
	}
	return out
}

// keepStoredPluginValues gives each key of cfg that has an empty value its
// stored value. The edit page never shows a stored value, so an empty value
// means "keep it". A key that is not in cfg is removed by the update.
func keepStoredPluginValues(cfg, stored map[string]string) {
	for k, v := range cfg {
		if old, ok := stored[k]; ok && v == "" {
			cfg[k] = old
		}
	}
}
