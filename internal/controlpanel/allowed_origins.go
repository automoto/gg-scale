package controlpanel

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/webutil"
)

func (h *Handler) maxProjectOrigins() int {
	if h.cfg.CORSMaxProjectOrigins > 0 {
		return h.cfg.CORSMaxProjectOrigins
	}
	return 20
}

// updateAllowedOriginsHandler saves the project's browser origins, one per
// line in the form.
func (h *Handler) updateAllowedOriginsHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, projectID, ok := h.parseTenantAndProject(w, r)
	if !ok {
		return
	}
	if !h.requireControlPanelPermission(w, r, tenantID, rbac.ProjectConfigObject(projectID), rbac.ActionUpdate) {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	raw := r.Form.Get("origins")
	session, _ := sessionFromContext(r.Context())
	_, err := projectadmin.SetAllowedOrigins(r.Context(), h.pool, tenantID, projectID,
		strings.Split(raw, "\n"), h.maxProjectOrigins(), projectadmin.Actor{UserID: session.User.ID})
	switch {
	case errors.Is(err, projectadmin.ErrInvalidOrigins):
		h.renderAllowedOriginsError(w, r, tenantID, projectID, raw, err.Error())
		return
	case errors.Is(err, pgx.ErrNoRows):
		http.NotFound(w, r)
		return
	case err != nil:
		webutil.InternalError(w, "allowed origins: update", err)
		return
	}
	http.Redirect(w, r, projectSettingsPathTpl(tenantID, projectID)+queryFlash+
		url.QueryEscape("Allowed origins saved. Each server uses the new list within "+
			projectadmin.OriginCacheTTL.String()+"."), http.StatusSeeOther)
}

func (h *Handler) renderAllowedOriginsError(w http.ResponseWriter, r *http.Request, tenantID, projectID int64, raw, msg string) {
	view, err := h.projectSettingsView(r.Context(), tenantID, projectID)
	if errors.Is(err, errProjectNotInTenant) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		webutil.InternalError(w, "allowed origins: load error page", err)
		return
	}
	session, _ := sessionFromContext(r.Context())
	view.UserEmail = session.User.Email
	view.CSRFToken = session.CSRFToken
	view.AllowedOrigins = raw
	view.FieldErrors = map[string]string{"origins": msg}
	w.WriteHeader(http.StatusUnprocessableEntity)
	webutil.Render(r, w, ProjectSettingsPage(view))
}
