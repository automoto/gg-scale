package controlpanel

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

// RevisionView is one kept settings revision in a history list.
type RevisionView struct {
	Revision  int64
	Source    string
	Actor     string
	Snapshot  string
	CreatedAt time.Time
}

func revisionViews(revs []projectadmin.Revision) []RevisionView {
	out := make([]RevisionView, 0, len(revs))
	for _, r := range revs {
		actor := "—"
		switch {
		case r.TokenID != nil:
			actor = "MCP token #" + strconv.FormatInt(*r.TokenID, 10)
		case r.ActorUserID != nil:
			actor = "User #" + strconv.FormatInt(*r.ActorUserID, 10)
		}
		out = append(out, RevisionView{
			Revision:  r.Revision,
			Source:    r.Source,
			Actor:     actor,
			Snapshot:  formatRemoteConfig(r.Snapshot),
			CreatedAt: r.CreatedAt,
		})
	}
	return out
}

// parseRollbackForm reads the revision to apply and the newest revision the
// page showed, so a rollback from a stale page is refused.
func parseRollbackForm(w http.ResponseWriter, r *http.Request) (revision, expected int64, ok bool) {
	if !webutil.ParseForm(w, r) {
		return 0, 0, false
	}
	revision, err1 := strconv.ParseInt(r.Form.Get("revision"), 10, 64)
	expected, err2 := strconv.ParseInt(r.Form.Get("expected_revision"), 10, 64)
	if err1 != nil || err2 != nil {
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return 0, 0, false
	}
	return revision, expected, true
}

// writeRollbackError maps a rollback or restore failure to a response. It
// reports whether err was nil.
func writeRollbackError(w http.ResponseWriter, r *http.Request, op string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, projectadmin.ErrRevisionNotFound):
		http.NotFound(w, r)
	case errors.Is(err, projectadmin.ErrRevisionConflict):
		http.Error(w, "The value changed after the page loaded. Reload the page and try again.", http.StatusConflict)
	case errors.Is(err, projectadmin.ErrDuplicateLeaderboard):
		http.Error(w, "A leaderboard with that name already exists in this Game Project. Rename it first.", http.StatusConflict)
	case errors.Is(err, projectadmin.ErrSortOrderLocked):
		http.Error(w, "Sort order is fixed once scores exist, so this revision cannot be applied.", http.StatusConflict)
	default:
		slog.ErrorContext(r.Context(), op+" failed", "err", err)
		http.Error(w, op+" failed", http.StatusInternalServerError)
	}
	return false
}

func (h *Handler) remoteConfigRollbackHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectConfigObject(projectID), rbac.ActionUpdate) {
		return
	}
	revision, expected, ok := parseRollbackForm(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	_, err := projectadmin.RollbackRemoteConfig(r.Context(), h.pool, tenantID, projectID, revision, &expected,
		projectadmin.Actor{UserID: session.User.ID})
	if !writeRollbackError(w, r, "remote config rollback", err) {
		return
	}
	http.Redirect(w, r, projectSettingsPathTpl(tenantID, projectID)+queryFlash+
		url.QueryEscape("Remote config rolled back to revision "+strconv.FormatInt(revision, 10)+"."), http.StatusSeeOther)
}

func (h *Handler) leaderboardRollbackHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	id, ok := parsePathID(w, r, "leaderboardID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectLeaderboardObject(projectID), rbac.ActionManage) {
		return
	}
	revision, expected, ok := parseRollbackForm(w, r)
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	_, err := projectadmin.RollbackLeaderboard(r.Context(), h.pool, tenantID, projectID, id, revision, &expected,
		projectadmin.Actor{UserID: session.User.ID})
	if !writeRollbackError(w, r, "leaderboard rollback", err) {
		return
	}
	http.Redirect(w, r, leaderboardsBasePath(tenantID, projectID)+"/"+strconv.FormatInt(id, 10)+queryFlash+
		url.QueryEscape("Settings rolled back to revision "+strconv.FormatInt(revision, 10)+". Scores did not change."), http.StatusSeeOther)
}

func (h *Handler) leaderboardRestoreHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	id, ok := parsePathID(w, r, "leaderboardID")
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectLeaderboardObject(projectID), rbac.ActionManage) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	err := projectadmin.RestoreLeaderboard(r.Context(), h.pool, tenantID, projectID, id, projectadmin.Actor{UserID: session.User.ID})
	if !writeRollbackError(w, r, "leaderboard restore", err) {
		return
	}
	http.Redirect(w, r, leaderboardsBasePath(tenantID, projectID)+queryFlash+url.QueryEscape("Leaderboard restored."), http.StatusSeeOther)
}
