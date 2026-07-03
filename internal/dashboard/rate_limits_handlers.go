package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ggscale/ggscale/internal/webutil"
)

func (h *Handler) rateLimitsPage(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	view, err := h.rateLimitsView(r.Context(), tenantID)
	if err != nil {
		http.Error(w, "rate limits load failed", http.StatusInternalServerError)
		return
	}
	view.UserEmail = session.User.Email
	view.CSRFToken = session.CSRFToken
	view.IsPlatformAdmin = session.User.IsPlatformAdmin
	view.Message = r.URL.Query().Get("flash")
	webutil.Render(r, w, RateLimitsPage(view))
}

// updateTenantAPILimitHandler sets the tenant-wide HTTP API override. Restricted
// to platform admins — tenant admins can't lift their own API ceiling.
func (h *Handler) updateTenantAPILimitHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	session, _ := sessionFromContext(r.Context())
	if !session.User.IsPlatformAdmin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	rate, rerr := parseLimitField(r.Form.Get("rate"))
	burst, berr := parseLimitField(r.Form.Get("burst"))
	if rerr != nil || berr != nil {
		h.redirectRateLimits(w, r, tenantID, "Rate and burst must be non-negative numbers.")
		return
	}
	if err := h.setTenantAPIOverride(r.Context(), session.User.ID, tenantID, rate, burst); err != nil {
		h.rateLimitError(w, r, tenantID, err)
		return
	}
	h.redirectRateLimits(w, r, tenantID, "API limit updated.")
}

// updateProjectInviteLimitHandler sets per-project invite quotas (tenant admin).
func (h *Handler) updateProjectInviteLimitHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	projectID, ok := parsePathID(w, r, "projectID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	inviterPerHour, ierr := parseLimitField(r.Form.Get("inviter_per_hour"))
	domainPerDay, derr := parseLimitField(r.Form.Get("domain_per_day"))
	if ierr != nil || derr != nil {
		h.redirectRateLimits(w, r, tenantID, "Invite quotas must be non-negative numbers.")
		return
	}
	session, _ := sessionFromContext(r.Context())
	if err := h.setProjectInviteOverride(r.Context(), session.User.ID, tenantID, projectID, inviterPerHour, domainPerDay); err != nil {
		h.rateLimitError(w, r, tenantID, err)
		return
	}
	h.redirectRateLimits(w, r, tenantID, "Invite quotas updated.")
}

func (h *Handler) rateLimitError(w http.ResponseWriter, r *http.Request, tenantID int64, err error) {
	switch {
	case errors.Is(err, errInvalidLimit):
		h.redirectRateLimits(w, r, tenantID, "Values must be finite non-negative numbers.")
	case errors.Is(err, errIncompleteLimit):
		h.redirectRateLimits(w, r, tenantID, "Enter both rate and burst, or clear both to restore the default.")
	case errors.Is(err, errExceedsCap):
		h.redirectRateLimits(w, r, tenantID, "Per-project invite quota can't exceed the tenant cap.")
	case errors.Is(err, errProjectNotInTenant):
		http.Error(w, "project not found", http.StatusNotFound)
	default:
		http.Error(w, "rate limit update failed", http.StatusInternalServerError)
	}
}

func (h *Handler) redirectRateLimits(w http.ResponseWriter, r *http.Request, tenantID int64, flash string) {
	target := pathTenantsPrefix + strconv.FormatInt(tenantID, 10) + "/rate-limits?flash=" + url.QueryEscape(flash)
	htmxRedirect(w, r, target)
}
