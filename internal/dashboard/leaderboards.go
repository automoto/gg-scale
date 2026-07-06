package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/webutil"
)

var errDuplicateLeaderboard = errors.New("dashboard: leaderboard with that name already exists")

const (
	sortOrderAsc  = "asc"
	sortOrderDesc = "desc"
)

// normalizeSortOrder trims and lowercases the submitted sort order, defaulting
// an empty value to "desc" (higher score ranks first). It reports ok=false for
// anything other than asc/desc so the handler surfaces a field error before the
// DB CHECK constraint would.
func normalizeSortOrder(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return sortOrderDesc, true
	case sortOrderAsc:
		return sortOrderAsc, true
	case sortOrderDesc:
		return sortOrderDesc, true
	default:
		return "", false
	}
}

// leaderboardsBasePath builds a project's leaderboard CRUD route prefix; it is
// shared by the handlers and the templates.
func leaderboardsBasePath(tenantID, projectID int64) string {
	return pathTenantsPrefix + strconv.FormatInt(tenantID, 10) +
		"/projects/" + strconv.FormatInt(projectID, 10) + "/leaderboards"
}

func (h *Handler) leaderboardsListPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	boards, err := h.listLeaderboards(r.Context(), tenantID, projectID)
	if err != nil {
		slog.ErrorContext(r.Context(), "leaderboards list failed", "err", err)
		http.Error(w, "leaderboards list failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, LeaderboardsListPage(LeaderboardsListView{
		UserEmail:    session.User.Email,
		CSRFToken:    session.CSRFToken,
		TenantID:     tenantID,
		ProjectID:    projectID,
		Leaderboards: boards,
		Message:      r.URL.Query().Get("flash"),
	}))
}

func (h *Handler) leaderboardsNewPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, NewLeaderboardPage(LeaderboardFormView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		TenantID:  tenantID,
		ProjectID: projectID,
		SortOrder: sortOrderDesc,
	}))
}

func (h *Handler) leaderboardsCreateHandler(w http.ResponseWriter, r *http.Request) {
	h.leaderboardFormHandler(w, r, false)
}

func (h *Handler) leaderboardsUpdateHandler(w http.ResponseWriter, r *http.Request) {
	h.leaderboardFormHandler(w, r, true)
}

// leaderboardFormHandler is the shared create/update form flow: parse,
// validate, check permission, save, audit, redirect with a flash. With
// edit=true it reads the leaderboard id from the path and updates; otherwise
// it creates.
func (h *Handler) leaderboardFormHandler(w http.ResponseWriter, r *http.Request, edit bool) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	var id int64
	if edit {
		if id, ok = parsePathID(w, r, "leaderboardID"); !ok {
			return
		}
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	page, action, verb := NewLeaderboardPage, "leaderboard.create", "created"
	if edit {
		page, action, verb = EditLeaderboardPage, "leaderboard.update", "updated"
	}
	session, _ := sessionFromContext(r.Context())
	name := strings.TrimSpace(r.Form.Get("name"))
	sortOrder, sortOK := normalizeSortOrder(r.Form.Get("sort_order"))
	view := LeaderboardFormView{
		UserEmail:     session.User.Email,
		CSRFToken:     session.CSRFToken,
		TenantID:      tenantID,
		ProjectID:     projectID,
		LeaderboardID: id,
		Name:          name,
		SortOrder:     sortOrder,
		FieldErrors:   map[string]string{},
	}
	validateLeaderboardForm(name, sortOK, view.FieldErrors)
	if !sortOK {
		view.SortOrder = sortOrderDesc
	}
	if len(view.FieldErrors) > 0 {
		w.WriteHeader(http.StatusUnprocessableEntity)
		webutil.Render(r, w, page(view))
		return
	}
	if !h.requireDashboardPermission(w, r, tenantID, rbac.ProjectLeaderboardObject(projectID), rbac.ActionManage) {
		return
	}
	var err error
	if edit {
		err = h.updateLeaderboard(r.Context(), tenantID, projectID, id, name, sortOrder)
	} else {
		id, err = h.createLeaderboard(r.Context(), tenantID, projectID, name, sortOrder)
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Deleted concurrently — nothing was written, so no success flash
		// and no audit row.
		http.NotFound(w, r)
		return
	case errors.Is(err, errDuplicateLeaderboard):
		view.FieldErrors["name"] = "A leaderboard with that name already exists."
		w.WriteHeader(http.StatusConflict)
		webutil.Render(r, w, page(view))
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "save leaderboard failed", "action", action, "err", err)
		view.Error = "Save failed."
		w.WriteHeader(http.StatusInternalServerError)
		webutil.Render(r, w, page(view))
		return
	}
	h.auditLeaderboard(r.Context(), tenantID, session.User.ID, action, id, map[string]any{
		"project_id":       projectID,
		"leaderboard_name": name,
	})
	http.Redirect(w, r, leaderboardsBasePath(tenantID, projectID)+queryFlash+url.QueryEscape("Leaderboard \""+name+"\" "+verb+"."), http.StatusSeeOther)
}

func (h *Handler) leaderboardsEditPage(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	id, ok := parsePathID(w, r, "leaderboardID")
	if !ok {
		return
	}
	row, err := h.getLeaderboard(r.Context(), tenantID, projectID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "leaderboard lookup failed", "err", err)
		http.Error(w, "leaderboard lookup failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	webutil.Render(r, w, EditLeaderboardPage(LeaderboardFormView{
		UserEmail:     session.User.Email,
		CSRFToken:     session.CSRFToken,
		TenantID:      tenantID,
		ProjectID:     projectID,
		LeaderboardID: row.ID,
		Name:          row.Name,
		SortOrder:     row.SortOrder,
	}))
}

func (h *Handler) leaderboardsDeleteHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	id, ok := parsePathID(w, r, "leaderboardID")
	if !ok {
		return
	}
	if !h.requireDashboardPermission(w, r, tenantID, rbac.ProjectLeaderboardObject(projectID), rbac.ActionManage) {
		return
	}
	err := h.softDeleteLeaderboard(r.Context(), tenantID, projectID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "delete leaderboard failed", "err", err)
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	session, _ := sessionFromContext(r.Context())
	h.auditLeaderboard(r.Context(), tenantID, session.User.ID, "leaderboard.delete", id, map[string]any{
		"project_id": projectID,
	})
	http.Redirect(w, r, leaderboardsBasePath(tenantID, projectID)+queryFlash+url.QueryEscape("Leaderboard deleted."), http.StatusSeeOther)
}

// validateLeaderboardForm collects field errors for the shared create/update
// form fields into errs.
func validateLeaderboardForm(name string, sortOK bool, errs map[string]string) {
	if name == "" {
		errs["name"] = "Name is required."
	}
	if !sortOK {
		errs["sort_order"] = "Sort order must be ascending or descending."
	}
}

// ── store helpers (inline sqlc, tenant-scoped) ──────────────────────────────

func (h *Handler) listLeaderboards(ctx context.Context, tenantID, projectID int64) ([]LeaderboardRowView, error) {
	if h.pool == nil {
		return nil, errors.New(msgDashboardPoolNeeded)
	}
	var out []LeaderboardRowView
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListLeaderboardsForProject(ctx, projectID)
		if err != nil {
			return fmt.Errorf("list leaderboards: %w", err)
		}
		out = make([]LeaderboardRowView, 0, len(rows))
		for _, row := range rows {
			v := LeaderboardRowView{ID: row.ID, Name: row.Name, SortOrder: row.SortOrder}
			if row.CreatedAt.Valid {
				v.CreatedAt = row.CreatedAt.Time
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

func (h *Handler) getLeaderboard(ctx context.Context, tenantID, projectID, id int64) (sqlcgen.GetLeaderboardForDashboardRow, error) {
	var row sqlcgen.GetLeaderboardForDashboardRow
	if h.pool == nil {
		return row, errors.New(msgDashboardPoolNeeded)
	}
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		row, err = sqlcgen.New(tx).GetLeaderboardForDashboard(ctx, sqlcgen.GetLeaderboardForDashboardParams{
			ProjectID: projectID,
			ID:        id,
		})
		return err
	})
	return row, err
}

func (h *Handler) createLeaderboard(ctx context.Context, tenantID, projectID int64, name, sortOrder string) (int64, error) {
	if h.pool == nil {
		return 0, errors.New(msgDashboardPoolNeeded)
	}
	var id int64
	ctx = db.WithTenant(ctx, tenantID)
	err := h.pool.Q(ctx, func(tx pgx.Tx) error {
		var err error
		id, err = sqlcgen.New(tx).CreateLeaderboard(ctx, sqlcgen.CreateLeaderboardParams{
			ProjectID: projectID,
			Name:      name,
			SortOrder: sortOrder,
		})
		return translateLeaderboardDuplicate(err)
	})
	return id, err
}

// updateLeaderboard renames/reorders a live leaderboard. It returns
// pgx.ErrNoRows when the row no longer exists (e.g. deleted concurrently), so
// callers never report success for a mutation that matched nothing.
func (h *Handler) updateLeaderboard(ctx context.Context, tenantID, projectID, id int64, name, sortOrder string) error {
	if h.pool == nil {
		return errors.New(msgDashboardPoolNeeded)
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).UpdateLeaderboard(ctx, sqlcgen.UpdateLeaderboardParams{
			Name:      name,
			SortOrder: sortOrder,
			ProjectID: projectID,
			ID:        id,
		})
		if err != nil {
			return translateLeaderboardDuplicate(err)
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// softDeleteLeaderboard hides a leaderboard; like updateLeaderboard it returns
// pgx.ErrNoRows when nothing matched.
func (h *Handler) softDeleteLeaderboard(ctx context.Context, tenantID, projectID, id int64) error {
	if h.pool == nil {
		return errors.New(msgDashboardPoolNeeded)
	}
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		n, err := sqlcgen.New(tx).SoftDeleteLeaderboard(ctx, sqlcgen.SoftDeleteLeaderboardParams{
			ProjectID: projectID,
			ID:        id,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

func translateLeaderboardDuplicate(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return errDuplicateLeaderboard
	}
	return err
}

// auditLeaderboard records a leaderboard mutation by a dashboard user in
// platform_audit_log (the actor is a dashboard_user, not a player). Audit
// failure is logged, never fatal to the request.
func (h *Handler) auditLeaderboard(ctx context.Context, tenantID, actorUserID int64, action string, id int64, payload map[string]any) {
	if err := h.writePlatformAudit(ctx, tenantID, actorUserID, action, strconv.FormatInt(id, 10), payload); err != nil {
		slog.WarnContext(ctx, "audit log: "+action, "err", err)
	}
}
