package controlpanel

import (
	"context"
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
		Message:           flashMessage(r),
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
		UserEmail:              session.User.Email,
		CSRFToken:              session.CSRFToken,
		TenantID:               tenantID,
		ProjectID:              projectID,
		BackendConfigured:      h.fleetBackendName(),
		AgonesProjectIsolation: h.agonesProjectIsolation(),
		Backend:                h.fleetBackendName(),
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

		AgonesProjectIsolation: h.agonesProjectIsolation(),
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
	if !h.requireControlPanelFleetManage(w, r, tenantID, projectID) {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	name := strings.TrimSpace(r.Form.Get("name"))
	backend := strings.TrimSpace(r.Form.Get("backend"))
	cfg, fieldErrors := parseFleetConfigForm(backend, r.Form)
	h.checkAgonesServerSetKeys(backend, cfg, fieldErrors)
	if name == "" {
		fieldErrors["name"] = "Name is required."
	}
	if backend == "" {
		fieldErrors["backend"] = "Backend is required."
	}
	view := NewFleetView{
		UserEmail:              session.User.Email,
		CSRFToken:              session.CSRFToken,
		TenantID:               tenantID,
		ProjectID:              projectID,
		BackendConfigured:      h.fleetBackendName(),
		AgonesProjectIsolation: h.agonesProjectIsolation(),
		Name:                   name,
		Backend:                backend,
		Config:                 fleetFormConfig(backend, cfg),
		Retry:                  true,
		FieldErrors:            fieldErrors,
	}
	if len(fieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, NewFleetPage(view))
		return
	}
	if !h.requireControlPanelBackendFeature(w, r, tenantID, projectID, backend) {
		return
	}

	_, err := h.fleet.FleetAdmin().CreateFleet(r.Context(), tenantID, fleet.FleetCreate{
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
	redirectFlash(w, r, fleetsBasePath(tenantID, projectID), flashFleetCreated)
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
		UserEmail:              session.User.Email,
		CSRFToken:              session.CSRFToken,
		TenantID:               tenantID,
		ProjectID:              projectID,
		FleetID:                f.ID,
		Name:                   f.Name,
		Backend:                f.Backend,
		Config:                 fleetFormConfig(f.Backend, f.Config),
		BackendConfigured:      h.fleetBackendName(),
		AgonesProjectIsolation: h.agonesProjectIsolation(),
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
	h.checkAgonesServerSetKeys(existing.Backend, cfg, fieldErrors)
	if fleet.ConfigIsSecret(existing.Backend) {
		if err := keepStoredPluginValues(r.Context(), cfg, existing); err != nil {
			fieldErrors["config_key"] = err.Error()
		}
	}
	if name == "" {
		fieldErrors["name"] = "Name is required."
	}
	view := EditFleetView{
		UserEmail:              session.User.Email,
		CSRFToken:              session.CSRFToken,
		TenantID:               tenantID,
		ProjectID:              projectID,
		FleetID:                fleetID,
		Name:                   name,
		Backend:                existing.Backend,
		Config:                 fleetFormConfig(existing.Backend, cfg),
		BackendConfigured:      h.fleetBackendName(),
		AgonesProjectIsolation: h.agonesProjectIsolation(),
		Retry:                  true,
		FieldErrors:            fieldErrors,
	}
	if len(fieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, EditFleetPage(view))
		return
	}
	if _, err := h.fleet.FleetAdmin().UpdateFleet(r.Context(), tenantID, projectID, fleetID, &name, cfg, projectadmin.Actor{UserID: session.User.ID}); err != nil {
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
	redirectFlash(w, r, fleetsBasePath(tenantID, projectID), flashFleetUpdated)
}

// fleetsDeleteHandler soft-deletes the template. Historical allocations
// keep their fleet_id reference; the control panel surfaces the (now-deleted)
// name from the row.
func (h *Handler) fleetsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, existing, ok := h.projectFleet(w, r, true)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	if err := h.fleet.FleetAdmin().DeleteFleet(r.Context(), tenantID, projectID, existing.ID, projectadmin.Actor{UserID: session.User.ID}); err != nil {
		if errors.Is(err, fleet.ErrFleetNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.ErrorContext(r.Context(), "delete fleet failed", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	redirectFlash(w, r, fleetsBasePath(tenantID, projectID), flashFleetDeleted)
}

// fleetsRestoreHandler brings back a deleted template. A live template with
// the same name blocks the restore.
func (h *Handler) fleetsRestoreHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, existing, ok := h.projectFleet(w, r, true)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	_, err := h.fleet.FleetAdmin().RestoreFleet(r.Context(), tenantID, projectID, existing.ID, projectadmin.Actor{UserID: session.User.ID})
	base := fleetsBasePath(tenantID, projectID)
	switch {
	case errors.Is(err, fleet.ErrFleetNotFound):
		http.NotFound(w, r)
	case errors.Is(err, fleet.ErrFleetNameTaken):
		redirectFlash(w, r, base, flashFleetNameTaken)
	case err != nil:
		slog.ErrorContext(r.Context(), "restore fleet failed", "err", err)
		http.Error(w, "restore failed", http.StatusInternalServerError)
	default:
		redirectFlash(w, r, base, flashFleetRestored)
	}
}

// projectFleet parses the IDs of a fleet route and returns the fleet (live or
// deleted) of the project. On failure it writes the answer and ok is false.
// For a write (mutate) it checks the fleet manage right before the lookup, so
// a role without it learns nothing about the fleet, then reads the form, and
// checks the backend grant of the stored fleet after the lookup.
func (h *Handler) projectFleet(w http.ResponseWriter, r *http.Request, mutate bool) (tenantID, projectID int64, f *fleet.Fleet, ok bool) {
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
	if mutate && !h.requireControlPanelFleetManage(w, r, tenantID, projectID) {
		return 0, 0, nil, false
	}
	if mutate && !webutil.ParseForm(w, r) {
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
	if mutate && !h.requireControlPanelBackendFeature(w, r, tenantID, projectID, f.Backend) {
		return 0, 0, nil, false
	}
	return tenantID, projectID, f, true
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
	case errors.Is(err, fleet.ErrLiveAllocationLimit):
		return prefix + ": " + fleet.ErrLiveAllocationLimit.Error() + ".", http.StatusConflict
	case errors.Is(err, fleet.ErrFleetBackendMismatch):
		return prefix + ": the fleet backend does not match the server backend.", http.StatusConflict
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
	pairs, overCap, duplicate := boundedKeyValuePairs(form["selector_key[]"], form["selector_value[]"])
	if overCap {
		errs["selector_key"] = "Too many selector keys."
	}
	if duplicate {
		errs["selector_key"] = "A selector key is in two rows. Keep one row for each key."
	}
	for k, v := range pairs {
		cfg["selector."+k] = v
	}
	if err := fleet.ValidateConfig("agones", cfg); err != nil {
		errs["fleet_name"] = err.Error()
	}
	return cfg, errs
}

// checkAgonesServerSetKeys adds a field error for each agones config key
// that the server sets when Agones project isolation is on. The fleet store
// refuses the same keys; this check shows the error at the field.
func (h *Handler) checkAgonesServerSetKeys(backend string, cfg, errs map[string]string) {
	if backend != "agones" || !h.agonesProjectIsolation() {
		return
	}
	for k := range cfg {
		if !fleet.AgonesServerSetKey(k) {
			continue
		}
		field := "selector_key"
		if k == "namespace" {
			field = "namespace"
		}
		errs[field] = fleet.ErrAgonesServerSetKey.Error()
	}
}

// agonesProjectIsolation reports whether the fleet store refuses the agones
// keys that the server sets (AGONES_PROJECT_ISOLATION).
func (h *Handler) agonesProjectIsolation() bool {
	if h.fleet == nil {
		return false
	}
	store := h.fleet.FleetAdmin()
	return store != nil && store.AgonesProjectIsolation
}

func parsePluginFleetConfig(form url.Values) (map[string]string, map[string]string) {
	cfg, overCap, duplicate := boundedKeyValuePairs(form["config_key[]"], form["config_value[]"])
	errs := map[string]string{}
	if overCap {
		errs["config_key"] = "Too many config keys."
	}
	if duplicate {
		errs["config_key"] = "A config key is in two rows. Keep one row for each key."
	}
	return cfg, errs
}

// boundedKeyValuePairs zips a key[] / value[] form pair into a map, enforcing
// the per-form count and per-entry length caps. Blank keys and entries past
// the value slice are skipped. overCap reports whether the key count was
// truncated so callers can surface a field error. duplicate reports a key in
// two rows, so no typed value is dropped without a word.
func boundedKeyValuePairs(keys, values []string) (pairs map[string]string, overCap, duplicate bool) {
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
		if _, ok := pairs[k]; ok {
			duplicate = true
		}
		pairs[k] = v
	}
	return pairs, overCap, duplicate
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

// keepStoredPluginValues gives each key of cfg that has an empty value the
// stored value of existing. The edit page never shows a stored value, so an
// empty value means "keep it". A key that is not in cfg is removed by the
// update. It returns fleet.ErrConfigNotOpened when a value must be kept and
// the stored config does not open.
func keepStoredPluginValues(ctx context.Context, cfg map[string]string, existing *fleet.Fleet) error {
	for k, v := range cfg {
		if _, ok := existing.Config[k]; !ok || v != "" {
			continue
		}
		stored, err := existing.OpenedConfig(ctx)
		if err != nil {
			return err
		}
		cfg[k] = stored[k]
	}
	return nil
}
