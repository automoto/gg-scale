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
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	fleetPageSize     = 25
	fleetEventLimit   = 50
	matchmakerMaxRows = 100
)

// fleetEnabled is true when a backend was wired at startup. The pages render
// a "not configured" empty state otherwise so operators know the manager is
// dormant rather than seeing a misleading "no allocations" message.
func (h *Handler) fleetEnabled() bool { return h.fleet != nil }

func (h *Handler) fleetListPage(w http.ResponseWriter, r *http.Request) {
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

// fleetListFragment serves the polled body of the fleet list. Same view model
// as the full page; the template renders only the table when invoked through
// this entry point.
func (h *Handler) fleetListFragment(w http.ResponseWriter, r *http.Request) {
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

func (h *Handler) fleetDetailPage(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "fleet detail failed", http.StatusInternalServerError)
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

func (h *Handler) fleetDetailFragment(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "fleet detail failed", http.StatusInternalServerError)
		return
	}
	webutil.Render(r, w, FleetDetailFragment(FleetDetailView{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Allocation: allocToView(alloc),
		Events:     events,
	}))
}

func (h *Handler) fleetNewPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, NewFleetAllocationPage(NewAllocationView{
		UserEmail:   session.User.Email,
		CSRFToken:   session.CSRFToken,
		TenantID:    tenantID,
		ProjectID:   projectID,
		BackendName: h.fleetBackendName(),
		Enabled:     h.fleetEnabled(),
	}))
}

func (h *Handler) fleetAllocateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	if !h.fleetEnabled() {
		http.Error(w, "no fleet backend configured", http.StatusServiceUnavailable)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	region := strings.TrimSpace(r.Form.Get("region"))
	gameMode := strings.TrimSpace(r.Form.Get("game_mode"))
	capacity, _ := strconv.Atoi(r.Form.Get("capacity"))
	if capacity <= 0 {
		capacity = 1
	}
	view := NewAllocationView{
		UserEmail: session.User.Email, CSRFToken: session.CSRFToken,
		TenantID: tenantID, ProjectID: projectID, BackendName: h.fleetBackendName(),
		Enabled: true, Region: region, GameMode: gameMode, Capacity: capacity,
	}
	if region == "" {
		view.FieldErrors = map[string]string{"region": "Region is required."}
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, NewFleetAllocationPage(view))
		return
	}

	tenantCtx := db.WithTenant(r.Context(), tenantID)
	alloc, err := h.fleet.Allocate(tenantCtx, fleet.AllocationRequest{
		TenantID:  tenantID,
		ProjectID: projectID,
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
		"region":     region,
		"game_mode":  gameMode,
		"capacity":   capacity,
	}); auditErr != nil {
		slog.WarnContext(r.Context(), "audit log: fleet.allocate.manual", "err", auditErr)
	}
	target := fleetBasePath(tenantID, projectID) + "/" + strconv.FormatInt(int64(alloc.ID), 10) +
		"?flash=" + url.QueryEscape("Allocation #"+strconv.FormatInt(int64(alloc.ID), 10)+" created.")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (h *Handler) fleetDeallocatePage(w http.ResponseWriter, r *http.Request) {
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
		http.Error(w, "fleet detail failed", http.StatusInternalServerError)
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

func (h *Handler) fleetDeallocateHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	allocID, ok := parsePathID(w, r, "allocID")
	if !ok {
		return
	}
	if !h.fleetEnabled() {
		http.Error(w, "no fleet backend configured", http.StatusServiceUnavailable)
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
			http.Error(w, "fleet detail failed", http.StatusInternalServerError)
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
	target := fleetBasePath(tenantID, projectID) + "?flash=" + url.QueryEscape("Allocation #"+strconv.FormatInt(allocID, 10)+" deallocated.")
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
	allocs, total, err := h.fleet.List(tenantCtx, projectID, includeTerminal, fleetPageSize, offset)
	if err != nil {
		return nil, 0, err
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
	return tenantID, projectID, true
}

func pageParam(r *http.Request) int {
	p, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || p <= 0 {
		return 1
	}
	return p
}

func fleetBasePath(tenantID, projectID int64) string {
	return "/v1/dashboard/tenants/" + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/fleet"
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
