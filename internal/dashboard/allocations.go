package dashboard

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/fleet"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	fleetPageSize     = 25
	fleetEventLimit   = 50
	matchmakerMaxRows = 100
	maxDashboardPage  = 100
)

// fleetEnabled is true when a backend was wired at startup. The pages render
// a "not configured" empty state otherwise so operators know the manager is
// dormant rather than seeing a misleading "no allocations" message.
func (h *Handler) fleetEnabled() bool { return h.fleet != nil }

func (h *Handler) allocationsListPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	includeTerminal := r.URL.Query().Get("all") == "1"
	page := pageParam(r)

	allocs, total, err := h.loadAllocations(r.Context(), tenantID, projectID, includeTerminal, page)
	if err != nil {
		http.Error(w, "fleet list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, FleetPage(FleetView{
		UserEmail:       session.User.Email,
		CSRFToken:       session.CSRFToken,
		TenantID:        tenantID,
		ProjectID:       projectID,
		BackendName:     h.fleetBackendName(),
		Enabled:         h.fleetEnabled(),
		IncludeTerminal: includeTerminal,
		Allocations:     allocs,
		Total:           total,
		Page:            page,
		HasPrev:         page > 1,
		HasNext:         int64(page*fleetPageSize) < total,
		Message:         r.URL.Query().Get("flash"),
	}))
}

// allocationsListFragment serves the polled body of the fleet list. Same view model
// as the full page; the template renders only the table when invoked through
// this entry point.
func (h *Handler) allocationsListFragment(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	includeTerminal := r.URL.Query().Get("all") == "1"
	page := pageParam(r)
	allocs, total, err := h.loadAllocations(r.Context(), tenantID, projectID, includeTerminal, page)
	if err != nil {
		http.Error(w, "fleet list failed", http.StatusInternalServerError)
		return
	}
	webutil.Render(r, w, FleetTableFragment(FleetView{
		TenantID:        tenantID,
		ProjectID:       projectID,
		IncludeTerminal: includeTerminal,
		Allocations:     allocs,
		Total:           total,
		Page:            page,
		HasPrev:         page > 1,
		HasNext:         int64(page*fleetPageSize) < total,
	}))
}

func (h *Handler) allocationsDetailPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	allocID, ok := parsePathID(w, r, "allocID")
	if !ok {
		return
	}
	alloc, events, err := h.loadAllocationDetail(r.Context(), tenantID, projectID, allocID)
	if errors.Is(err, fleet.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, msgFleetDetailFailed, http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, FleetDetailPage(FleetDetailView{
		UserEmail:  session.User.Email,
		CSRFToken:  session.CSRFToken,
		TenantID:   tenantID,
		ProjectID:  projectID,
		Allocation: allocToView(alloc),
		Events:     events,
		Message:    r.URL.Query().Get("flash"),
	}))
}

func (h *Handler) allocationsDetailFragment(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	allocID, ok := parsePathID(w, r, "allocID")
	if !ok {
		return
	}
	alloc, events, err := h.loadAllocationDetail(r.Context(), tenantID, projectID, allocID)
	if errors.Is(err, fleet.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, msgFleetDetailFailed, http.StatusInternalServerError)
		return
	}
	webutil.Render(r, w, FleetDetailFragment(FleetDetailView{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Allocation: allocToView(alloc),
		Events:     events,
	}))
}

func (h *Handler) allocationsNewPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	view := NewAllocationView{
		UserEmail:   session.User.Email,
		CSRFToken:   session.CSRFToken,
		TenantID:    tenantID,
		ProjectID:   projectID,
		BackendName: h.fleetBackendName(),
		Enabled:     h.fleetEnabled(),
	}
	if h.fleetEnabled() {
		view.Fleets = h.loadFleetOptions(r.Context(), tenantID, projectID)
	}
	webutil.Render(r, w, NewFleetAllocationPage(view))
}

func (h *Handler) allocationsAllocateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	if !h.fleetEnabled() {
		http.Error(w, msgNoFleetBackend, http.StatusServiceUnavailable)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	fleetName := strings.TrimSpace(r.Form.Get("fleet"))
	region := strings.TrimSpace(r.Form.Get("region"))
	gameMode := strings.TrimSpace(r.Form.Get("game_mode"))
	capacity, _ := strconv.Atoi(r.Form.Get("capacity"))
	if capacity <= 0 {
		capacity = 1
	}
	tenantCtx := db.WithTenant(r.Context(), tenantID)
	view := NewAllocationView{
		UserEmail: session.User.Email, CSRFToken: session.CSRFToken,
		TenantID: tenantID, ProjectID: projectID, BackendName: h.fleetBackendName(),
		Enabled: true, Fleet: fleetName, Region: region, GameMode: gameMode, Capacity: capacity,
		Fleets: h.loadFleetOptions(r.Context(), tenantID, projectID),
	}
	fieldErrors := map[string]string{}
	if fleetName == "" {
		fieldErrors["fleet"] = "Fleet is required."
	}
	if region == "" {
		fieldErrors["region"] = "Region is required."
	}
	if len(fieldErrors) > 0 {
		view.FieldErrors = fieldErrors
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, NewFleetAllocationPage(view))
		return
	}
	if !h.requireDashboardAllocationMutation(w, r, tenantID, projectID, rbac.ActionAllocate) {
		return
	}

	f, ferr := h.fleet.Fleets().GetByName(tenantCtx, projectID, fleetName)
	if errors.Is(ferr, fleet.ErrFleetNotFound) {
		view.FieldErrors = map[string]string{"fleet": "Unknown fleet for this project."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, NewFleetAllocationPage(view))
		return
	}
	if ferr != nil {
		slog.ErrorContext(r.Context(), msgFleetLookupFailed, "err", ferr, "fleet", fleetName)
		view.Error = "Fleet lookup failed: " + ferr.Error()
		w.WriteHeader(http.StatusInternalServerError)
		webutil.Render(r, w, NewFleetAllocationPage(view))
		return
	}

	alloc, err := h.fleet.Allocate(tenantCtx, fleet.AllocationRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
		FleetID:   f.ID,
		Region:    region,
		GameMode:  gameMode,
		Capacity:  capacity,
	})
	if err != nil {
		slog.ErrorContext(r.Context(), "manual allocate failed", "err", err, "tenant", tenantID, "project", projectID)
		view.Error = "Allocate failed: " + err.Error()
		w.WriteHeader(http.StatusInternalServerError)
		webutil.Render(r, w, NewFleetAllocationPage(view))
		return
	}
	if auditErr := h.writeFleetAudit(r.Context(), tenantID, session.User.ID, "fleet.allocate.manual", strconv.FormatInt(int64(alloc.ID), 10), map[string]any{
		"project_id": projectID,
		"fleet_id":   f.ID,
		"fleet_name": f.Name,
		"region":     region,
		"game_mode":  gameMode,
		"capacity":   capacity,
	}); auditErr != nil {
		slog.WarnContext(r.Context(), "audit log: fleet.allocate.manual", "err", auditErr)
	}
	target := allocationsBasePath(tenantID, projectID) + "/" + strconv.FormatInt(int64(alloc.ID), 10) +
		queryFlash + url.QueryEscape("Allocation #"+strconv.FormatInt(int64(alloc.ID), 10)+" created.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// loadFleetOptions fetches active fleet templates for the project so the
// manual-allocate form can render a dropdown. Returns an empty slice on
// error so the form still renders (with no fleets to pick from).
func (h *Handler) loadFleetOptions(ctx context.Context, tenantID, projectID int64) []FleetOption {
	if h.fleet == nil {
		return nil
	}
	tenantCtx := db.WithTenant(ctx, tenantID)
	rows, err := h.fleet.Fleets().ListForProject(tenantCtx, projectID)
	if err != nil {
		slog.ErrorContext(ctx, "list fleets for allocation form", "err", err)
		return nil
	}
	out := make([]FleetOption, 0, len(rows))
	backend := h.fleetBackendName()
	for _, f := range rows {
		out = append(out, FleetOption{
			ID:                f.ID,
			Name:              f.Name,
			Backend:           f.Backend,
			BackendMatches:    f.Backend == backend,
			BackendConfigured: backend,
		})
	}
	return out
}

func (h *Handler) allocationsDeallocatePage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	allocID, ok := parsePathID(w, r, "allocID")
	if !ok {
		return
	}
	alloc, _, err := h.loadAllocationDetail(r.Context(), tenantID, projectID, allocID)
	if errors.Is(err, fleet.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, msgFleetDetailFailed, http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, DeallocateConfirmPage(DeallocateConfirmView{
		UserEmail:  session.User.Email,
		CSRFToken:  session.CSRFToken,
		TenantID:   tenantID,
		ProjectID:  projectID,
		Allocation: allocToView(alloc),
	}))
}

func (h *Handler) allocationsDeallocateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	allocID, ok := parsePathID(w, r, "allocID")
	if !ok {
		return
	}
	if !h.fleetEnabled() {
		http.Error(w, msgNoFleetBackend, http.StatusServiceUnavailable)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	typed := strings.TrimSpace(r.Form.Get("confirm_id"))
	if typed != strconv.FormatInt(allocID, 10) {
		session, _ := sessionFromContext(r.Context())
		alloc, _, err := h.loadAllocationDetail(r.Context(), tenantID, projectID, allocID)
		if err != nil {
			http.Error(w, msgFleetDetailFailed, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, DeallocateConfirmPage(DeallocateConfirmView{
			UserEmail: session.User.Email, CSRFToken: session.CSRFToken,
			TenantID: tenantID, ProjectID: projectID, Allocation: allocToView(alloc),
			Error: "Typed ID did not match. Type the allocation ID exactly to confirm.",
		}))
		return
	}

	tenantCtx := db.WithTenant(r.Context(), tenantID)
	if !h.requireDashboardAllocationMutation(w, r, tenantID, projectID, rbac.ActionDeallocate) {
		return
	}
	if err := h.fleet.Deallocate(tenantCtx, fleet.AllocationID(allocID)); err != nil {
		slog.ErrorContext(r.Context(), "manual deallocate failed", "err", err, "alloc", allocID)
		http.Error(w, "deallocate failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	if auditErr := h.writeFleetAudit(r.Context(), tenantID, session.User.ID, "fleet.deallocate.manual",
		strconv.FormatInt(allocID, 10), map[string]any{"project_id": projectID}); auditErr != nil {
		slog.WarnContext(r.Context(), "audit log: fleet.deallocate.manual", "err", auditErr)
	}
	target := allocationsBasePath(tenantID, projectID) + queryFlash + url.QueryEscape("Allocation #"+strconv.FormatInt(allocID, 10)+" deallocated.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// fleetBackendsPage shows the backends seen across this tenant's allocations
// and the configured manager's health probe.
func (h *Handler) fleetBackendsPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	view := FleetBackendsView{
		UserEmail:      session.User.Email,
		CSRFToken:      session.CSRFToken,
		TenantID:       tenantID,
		ConfiguredName: h.fleetBackendName(),
		Enabled:        h.fleetEnabled(),
	}
	if !h.fleetEnabled() {
		webutil.Render(r, w, FleetBackendsPage(view))
		return
	}
	tenantCtx := db.WithTenant(r.Context(), tenantID)
	backends, err := h.fleet.BackendsForTenant(tenantCtx)
	if err != nil {
		http.Error(w, "backend list failed", http.StatusInternalServerError)
		return
	}
	for _, b := range backends {
		view.Backends = append(view.Backends, BackendRowView{Name: b.Name, AllocationCount: b.AllocationCount})
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.fleet.Backend().HealthCheck(probeCtx); err != nil {
		view.HealthErr = err.Error()
	}
	webutil.Render(r, w, FleetBackendsPage(view))
}

func (h *Handler) matchmakerQueuePage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	buckets, err := h.loadMatchmakerBuckets(r.Context(), tenantID, projectID)
	if err != nil {
		http.Error(w, "matchmaker queue failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, MatchmakerQueuePage(MatchmakerQueueView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
		Buckets:   buckets,
	}))
}

func (h *Handler) matchmakerQueueFragment(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	buckets, err := h.loadMatchmakerBuckets(r.Context(), tenantID, projectID)
	if err != nil {
		http.Error(w, "matchmaker queue failed", http.StatusInternalServerError)
		return
	}
	webutil.Render(r, w, MatchmakerTableFragment(MatchmakerQueueView{
		TenantID: tenantID, ProjectID: projectID, Buckets: buckets,
	}))
}

func (h *Handler) platformPluginsPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	view := PlatformPluginsView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
	}
	if h.pluginInfo != nil {
		view.Snapshot = h.pluginInfo()
	}
	webutil.Render(r, w, PlatformPluginsPage(view))
}

// loadAllocations runs the paginated fleet list inside a tenant-scoped tx.
func (h *Handler) loadAllocations(ctx context.Context, tenantID, projectID int64, includeTerminal bool, page int) ([]AllocationView, int64, error) {
	if !h.fleetEnabled() {
		return nil, 0, nil
	}
	tenantCtx := db.WithTenant(ctx, tenantID)
	offset := (page - 1) * fleetPageSize
	allocs, total, err := h.fleet.List(tenantCtx, projectID, includeTerminal, fleetPageSize+1, offset)
	if err != nil {
		return nil, 0, err
	}
	if len(allocs) > fleetPageSize {
		allocs = allocs[:fleetPageSize]
		total = int64(offset + fleetPageSize + 1)
	}
	out := make([]AllocationView, 0, len(allocs))
	for _, a := range allocs {
		out = append(out, allocToView(a))
	}
	return out, total, nil
}

func (h *Handler) loadAllocationDetail(ctx context.Context, tenantID, projectID, allocID int64) (*fleet.Allocation, []EventView, error) {
	if !h.fleetEnabled() {
		return nil, nil, errors.New("fleet not configured")
	}
	tenantCtx := db.WithTenant(ctx, tenantID)
	alloc, err := h.fleet.Get(tenantCtx, fleet.AllocationID(allocID))
	if err != nil {
		return nil, nil, err
	}
	if alloc.ProjectID != projectID {
		return nil, nil, fleet.ErrNotFound
	}
	events, err := h.fleet.ListEvents(tenantCtx, fleet.AllocationID(allocID), fleetEventLimit)
	if err != nil {
		return nil, nil, err
	}
	views := make([]EventView, 0, len(events))
	for _, e := range events {
		views = append(views, EventView{
			ID:         e.ID,
			Status:     e.Status.String(),
			Address:    e.Address,
			ErrMessage: e.ErrMessage,
			CreatedAt:  e.CreatedAt,
		})
	}
	return alloc, views, nil
}

func (h *Handler) loadMatchmakerBuckets(ctx context.Context, tenantID, projectID int64) ([]MatchmakerBucketView, error) {
	tenantCtx := db.WithTenant(ctx, tenantID)
	var out []MatchmakerBucketView
	err := h.pool.Q(tenantCtx, func(tx pgx.Tx) error {
		rows, qerr := sqlcgen.New(tx).ListMatchmakerBucketsForProject(ctx, projectID)
		if qerr != nil {
			return qerr
		}
		for _, row := range rows {
			out = append(out, MatchmakerBucketView{
				Region:   row.Region,
				GameMode: row.GameMode,
				Status:   row.Status,
				Count:    row.TicketCount,
				Oldest:   row.Oldest.Time,
			})
			if len(out) >= matchmakerMaxRows {
				break
			}
		}
		return nil
	})
	return out, err
}

// writeFleetAudit records a manual fleet action by a dashboard user.
// platform_audit_log is the correct table here: the actor is a
// dashboard_user (not an end_user), and the tenant FK on audit_log would
// reject it. We fold tenant_id + project_id into the payload so the row
// is still correlatable to a tenant.
func (h *Handler) writeFleetAudit(ctx context.Context, tenantID, actorUserID int64, action, target string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["tenant_id"] = tenantID
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		return auditlog.WritePlatform(ctx, tx, actorUserID, action, target, payload)
	})
}

// fleetBackendName returns the backend display name, or "" if no manager.
func (h *Handler) fleetBackendName() string {
	if h.fleet == nil {
		return ""
	}
	if b := h.fleet.Backend(); b != nil {
		return b.Name()
	}
	return ""
}

func (h *Handler) parseTenantAndProject(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return 0, 0, false
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return 0, 0, false
	}
	ok, err := h.projectBelongsToTenant(r.Context(), tenantID, projectID)
	if err != nil {
		slog.ErrorContext(r.Context(), "project ownership check failed", "err", err, "tenant", tenantID, "project", projectID)
		http.Error(w, "project lookup failed", http.StatusInternalServerError)
		return 0, 0, false
	}
	if !ok {
		http.NotFound(w, r)
		return 0, 0, false
	}
	return tenantID, projectID, true
}

func pageParam(r *http.Request) int {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p <= 0 {
		return 1
	}
	if p > maxDashboardPage {
		return maxDashboardPage
	}
	return p
}

func dashboardPageOffset(page, pageSize int) int32 {
	offset := (page - 1) * pageSize
	if offset <= 0 {
		return 0
	}
	// #nosec G115 -- pageParam clamps page to maxDashboardPage and dashboard page sizes are small constants.
	return int32(offset)
}

func dashboardPageLimit(pageSize int) int32 {
	// #nosec G115 -- dashboard page sizes are small constants and +1 is the LIMIT+1 sentinel.
	return int32(pageSize + 1)
}

func (h *Handler) projectBelongsToTenant(ctx context.Context, tenantID, projectID int64) (bool, error) {
	var belongs bool
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", stringFromInt(tenantID)); err != nil {
			return err
		}
		row, err := sqlcgen.New(tx).GetProjectTenant(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		belongs = row.TenantID == tenantID
		return nil
	})
	return belongs, err
}

func allocationsBasePath(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/allocations"
}

func fleetsBasePath(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/fleets"
}

func allocToView(a *fleet.Allocation) AllocationView {
	if a == nil {
		return AllocationView{}
	}
	return AllocationView{
		ID:         int64(a.ID),
		ProjectID:  a.ProjectID,
		Backend:    a.Backend,
		BackendRef: a.BackendRef,
		Region:     a.Region,
		Address:    a.Address,
		Status:     a.Status.String(),
	}
}
