package controlpanel

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

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/tenant"
	"github.com/ggscale/ggscale/internal/webutil"
)

const (
	pathAdminChangeRequests   = pathControlPanel + "/admin/change-requests"
	auditChangeRequestApprove = "control_panel.change_request.approve"
	auditChangeRequestDeny    = "control_panel.change_request.deny"

	changeKindTierUpgrade = "tier_upgrade"
	changeKindFeature     = "feature"
)

// errNotAnUpgrade rejects a tier-upgrade request whose target class is not
// strictly above the tenant's current class. The submit form only offers
// above-current classes, but a crafted POST can request any 0..3 value; without
// this guard an approval would silently downgrade the tenant's quotas/caps.
var errNotAnUpgrade = errors.New("requested tier is not above the current tier")

// tierIsUpgrade reports whether requested is a strictly higher class than the
// tenant's current (clamped) class — the only valid direction for a
// tier-upgrade request.
func tierIsUpgrade(requested, current int16) bool {
	return int(requested) > int(tenant.ClampTier(int(current)))
}

// requestableFeatures are the tenant-facing features a tenant may request. Each
// is offered only when its server-side env switch is on and the tenant doesn't
// already hold it. The fleet backends are governed by the same switch as
// dedicated_servers, so only the umbrella feature is offered here.
var requestableFeatures = []struct{ Value, Label string }{
	{"p2p_relay", "P2P relay (TURN)"},
	{"dedicated_servers", "Dedicated game-server fleets"},
}

// featureEnabledByEnv reports whether a requestable feature's server-side kill
// switch is on, so the tenant isn't offered a feature the server can't serve.
func (h *Handler) featureEnabledByEnv(feature string) bool {
	switch feature {
	case "p2p_relay":
		return h.cfg.RelayEnabled
	case "dedicated_servers", "fleet_docker_backend", "fleet_agones_backend", "fleet_plugin_backend":
		return h.cfg.FleetEnabled
	default:
		return false
	}
}

// loadChangeRequestSection populates the change-request part of the tenant
// settings view: the tenant's own requests, the upgrade target classes (only
// those above current), and the features it may still request. Runs in tenant
// RLS context so it can read feature_grants.
func (h *Handler) loadChangeRequestSection(ctx context.Context, tenantID int64, current tenant.Tier, view *TenantSettingsView) error {
	tctx := db.WithTenant(ctx, tenantID)
	return h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		rows, err := q.ListTenantChangeRequests(tctx, tenantID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			view.ChangeRequests = append(view.ChangeRequests, changeRequestView(row))
		}

		held, err := q.ListTenantEnabledFeatures(tctx)
		if err != nil {
			return err
		}
		heldSet := make(map[string]struct{}, len(held))
		for _, f := range held {
			heldSet[f] = struct{}{}
		}
		for _, f := range requestableFeatures {
			if _, ok := heldSet[f.Value]; ok || !h.featureEnabledByEnv(f.Value) {
				continue
			}
			view.FeatureOptions = append(view.FeatureOptions, FeatureOptionView{Value: f.Value, Label: f.Label})
		}

		for t := int(current) + 1; t <= int(tenant.Tier3); t++ {
			view.UpgradeTargets = append(view.UpgradeTargets, FeatureOptionView{
				Value: strconv.Itoa(t),
				Label: tenant.Tier(t).String(),
			})
		}
		view.CanRequestUpgrade = len(view.UpgradeTargets) > 0
		return nil
	})
}

func changeRequestView(row sqlcgen.ListTenantChangeRequestsRow) ChangeRequestView {
	v := ChangeRequestView{
		Kind:      row.Kind,
		Note:      row.Note,
		Status:    row.Status,
		CreatedAt: row.CreatedAt.Time,
	}
	switch {
	case row.Kind == changeKindTierUpgrade && row.RequestedTier != nil:
		v.Detail = tenant.ClampTier(int(*row.RequestedTier)).String()
	case row.Feature != nil:
		v.Detail = *row.Feature
	}
	if row.ReviewReason != nil {
		v.ReviewReason = *row.ReviewReason
	}
	return v
}

// submitChangeRequestHandler is the tenant-side submit for a tier upgrade or a
// feature request. Gated by requireTenantAccess(roleAdmin). The pending-unique
// index is the real anti-spam guard; a duplicate open request is rejected with
// a friendly message.
func (h *Handler) submitChangeRequestHandler(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parsePathID(w, r, "tenantID")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	kind := r.Form.Get("kind")
	note := strings.TrimSpace(r.Form.Get("note"))

	params := sqlcgen.CreateTenantChangeRequestParams{
		TenantID:          tenantID,
		RequestedByUserID: &session.User.ID,
		Kind:              kind,
		Note:              note,
	}
	switch kind {
	case changeKindTierUpgrade:
		tierStr := r.Form.Get("requested_tier")
		tierNum, perr := parseRequestedTier(tierStr)
		if perr != nil {
			h.redirectTenantSettings(w, r, tenantID, "Choose a valid target class to upgrade to.")
			return
		}
		params.RequestedTier = &tierNum
	case changeKindFeature:
		feature := r.Form.Get("feature")
		if !h.isRequestableFeature(feature) {
			h.redirectTenantSettings(w, r, tenantID, "Choose a feature that's available to request.")
			return
		}
		params.Feature = &feature
	default:
		h.redirectTenantSettings(w, r, tenantID, "Unknown request type.")
		return
	}

	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if kind == changeKindTierUpgrade {
			facts, ferr := q.GetTenantFacts(r.Context(), tenantID)
			if ferr != nil {
				return ferr
			}
			if !tierIsUpgrade(*params.RequestedTier, facts.Tier) {
				return errNotAnUpgrade
			}
		}
		_, cerr := q.CreateTenantChangeRequest(r.Context(), params)
		return cerr
	})
	if errors.Is(err, errNotAnUpgrade) {
		h.redirectTenantSettings(w, r, tenantID, "Choose a target class above your current class.")
		return
	}
	if isUniqueViolation(err) {
		h.redirectTenantSettings(w, r, tenantID, "You already have a pending request of this type.")
		return
	}
	if err != nil {
		webutil.InternalError(w, "change request: create", err)
		return
	}
	h.redirectTenantSettings(w, r, tenantID, "Request submitted. A platform admin will review it.")
}

func (h *Handler) isRequestableFeature(feature string) bool {
	for _, f := range requestableFeatures {
		if f.Value == feature {
			return h.featureEnabledByEnv(feature)
		}
	}
	return false
}

func parseRequestedTier(s string) (int16, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < int(tenant.Tier0) || n > int(tenant.Tier3) {
		return 0, errors.New("invalid tier")
	}
	return int16(n), nil //nolint:gosec // bounded to 0..3 by the check above
}

func (h *Handler) redirectTenantSettings(w http.ResponseWriter, r *http.Request, tenantID int64, msg string) {
	htmxRedirect(w, r, pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+"/settings"+queryFlash+url.QueryEscape(msg))
}

// --- platform-admin side ---------------------------------------------------

func (h *Handler) changeRequestsPage(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionFromContext(r.Context())
	var rows []sqlcgen.ListPendingTenantChangeRequestsRow
	if err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		var e error
		rows, e = sqlcgen.New(tx).ListPendingTenantChangeRequests(r.Context())
		return e
	}); err != nil {
		webutil.InternalError(w, "change requests: list", err)
		return
	}
	reqs := make([]PendingChangeRequestView, 0, len(rows))
	for _, row := range rows {
		reqs = append(reqs, pendingChangeRequestView(row))
	}
	webutil.Render(r, w, ChangeRequestsPage(ChangeRequestsView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Requests:  reqs,
		Message:   r.URL.Query().Get("flash"),
	}))
}

func pendingChangeRequestView(row sqlcgen.ListPendingTenantChangeRequestsRow) PendingChangeRequestView {
	v := PendingChangeRequestView{
		ID:          row.ID,
		TenantID:    row.TenantID,
		TenantName:  row.TenantName,
		CurrentTier: tenant.ClampTier(int(row.CurrentTier)).String(),
		Kind:        row.Kind,
		Note:        row.Note,
		CreatedAt:   row.CreatedAt.Time,
	}
	switch {
	case row.Kind == changeKindTierUpgrade && row.RequestedTier != nil:
		v.Target = tenant.ClampTier(int(*row.RequestedTier)).String()
	case row.Feature != nil:
		v.Target = *row.Feature
	}
	return v
}

// approveChangeRequestHandler approves a pending request and auto-applies it in
// one tenant-context tx: a tier upgrade updates tenants.tier; a feature request
// upserts an enabled tenant-level feature_grants row. Emails the tenant admins.
func (h *Handler) approveChangeRequestHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(w, r, "id")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())

	var applied sqlcgen.GetTenantChangeRequestByIDRow
	var handled bool
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		req, gerr := sqlcgen.New(tx).GetTenantChangeRequestByID(r.Context(), id)
		if errors.Is(gerr, pgx.ErrNoRows) {
			return nil
		}
		if gerr != nil {
			return gerr
		}
		if req.Status != "pending" {
			return nil
		}
		applied = req
		handled = true
		return nil
	})
	if err != nil {
		webutil.InternalError(w, "change request: load", err)
		return
	}
	if !handled {
		h.redirectChangeAdmin(w, r, "That request was already handled.")
		return
	}

	// Apply in the tenant's RLS context so the tenants/feature_grants writes
	// pass row-level security; the request-status update and audit are
	// context-independent.
	tctx := db.WithTenant(r.Context(), applied.TenantID)
	err = h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		n, aerr := q.ApproveTenantChangeRequest(tctx, sqlcgen.ApproveTenantChangeRequestParams{
			ReviewedBy: &session.User.ID,
			ID:         id,
		})
		if aerr != nil {
			return aerr
		}
		if n == 0 {
			handled = false
			return nil
		}
		if aerr := h.applyChangeRequest(tctx, q, applied, session.User.ID); aerr != nil {
			return aerr
		}
		return auditlog.WritePlatform(tctx, tx, session.User.ID, auditChangeRequestApprove,
			strconv.FormatInt(id, 10), map[string]any{
				"tenant_id": applied.TenantID,
				"kind":      applied.Kind,
			})
	})
	if err != nil {
		if errors.Is(err, errNotAnUpgrade) {
			h.redirectChangeAdmin(w, r, "The tenant is already at or above the requested class; the request was not approved.")
			return
		}
		webutil.InternalError(w, "change request: approve", err)
		return
	}
	if !handled {
		h.redirectChangeAdmin(w, r, "That request was already handled.")
		return
	}
	h.sendChangeRequestDecisionEmail(r.Context(), applied, true, "")
	h.reloadRBACPolicy(r.Context())
	h.redirectChangeAdmin(w, r, "Request approved and applied.")
}

func (h *Handler) applyChangeRequest(ctx context.Context, q *sqlcgen.Queries, req sqlcgen.GetTenantChangeRequestByIDRow, actorID int64) error {
	switch {
	case req.Kind == changeKindTierUpgrade && req.RequestedTier != nil:
		n, err := q.SetTenantTierIfUpgrade(ctx, *req.RequestedTier)
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotAnUpgrade
		}
		return nil
	case req.Kind == changeKindFeature && req.Feature != nil:
		return q.UpsertTenantFeatureGrant(ctx, sqlcgen.UpsertTenantFeatureGrantParams{
			Feature:    *req.Feature,
			ApprovedBy: &actorID,
			Reason:     strPtr("approved change request " + strconv.FormatInt(req.ID, 10)),
		})
	default:
		return fmt.Errorf("change request %d has an inconsistent shape", req.ID)
	}
}

func (h *Handler) denyChangeRequestHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePathID(w, r, "id")
	if !ok {
		return
	}
	if !webutil.ParseForm(w, r) {
		return
	}
	session, _ := sessionFromContext(r.Context())
	reason := strings.TrimSpace(r.Form.Get("reason"))
	var reasonPtr *string
	if reason != "" {
		reasonPtr = &reason
	}

	var req sqlcgen.GetTenantChangeRequestByIDRow
	var affected int64
	err := h.pool.BootstrapQ(r.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		n, derr := q.DenyTenantChangeRequest(r.Context(), sqlcgen.DenyTenantChangeRequestParams{
			ReviewedBy:   &session.User.ID,
			ReviewReason: reasonPtr,
			ID:           id,
		})
		if derr != nil {
			return derr
		}
		affected = n
		if n == 0 {
			return nil
		}
		got, gerr := q.GetTenantChangeRequestByID(r.Context(), id)
		if gerr != nil {
			return gerr
		}
		req = got
		return auditlog.WritePlatform(r.Context(), tx, session.User.ID, auditChangeRequestDeny,
			strconv.FormatInt(id, 10), map[string]any{
				"tenant_id": got.TenantID,
				"kind":      got.Kind,
				"reason":    reason,
			})
	})
	if err != nil {
		webutil.InternalError(w, "change request: deny", err)
		return
	}
	if affected == 0 {
		h.redirectChangeAdmin(w, r, "That request was already handled.")
		return
	}
	h.sendChangeRequestDecisionEmail(r.Context(), req, false, reason)
	h.redirectChangeAdmin(w, r, "Request denied.")
}

func (h *Handler) redirectChangeAdmin(w http.ResponseWriter, r *http.Request, msg string) {
	htmxRedirect(w, r, pathAdminChangeRequests+queryFlash+url.QueryEscape(msg))
}

// sendChangeRequestDecisionEmail notifies the tenant's owner/admins of an
// approve/deny outcome. Mirrors the signup decision emails.
func (h *Handler) sendChangeRequestDecisionEmail(ctx context.Context, req sqlcgen.GetTenantChangeRequestByIDRow, approved bool, reason string) {
	if h.mailer == nil || h.cfg.MailFrom == "" {
		return
	}
	var emails []string
	if err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var e error
		emails, e = sqlcgen.New(tx).ListTenantAdminEmails(ctx, req.TenantID)
		return e
	}); err != nil {
		slog.ErrorContext(ctx, "change request: list admin emails", "err", err, "tenant_id", req.TenantID)
		return
	}
	if len(emails) == 0 {
		return
	}
	subject, body := changeRequestDecisionEmail(req, approved, reason)
	if err := h.mailer.Send(ctx, mailer.Message{From: h.cfg.MailFrom, To: emails, Subject: subject, Body: body}); err != nil {
		slog.ErrorContext(ctx, "change request: decision mailer", "err", err, "tenant_id", req.TenantID)
	}
}

func changeRequestDecisionEmail(req sqlcgen.GetTenantChangeRequestByIDRow, approved bool, reason string) (subject, body string) {
	what := "feature " + deref(req.Feature)
	if req.Kind == changeKindTierUpgrade && req.RequestedTier != nil {
		what = "upgrade to " + tenant.ClampTier(int(*req.RequestedTier)).String()
	}
	if approved {
		return "Your ggscale change request was approved",
			fmt.Sprintf("Your request (%s) was approved and applied to your tenant.", what)
	}
	body = fmt.Sprintf("Your request (%s) was not approved at this time.", what)
	if reason != "" {
		body += "\n\nReason: " + reason
	}
	return "About your ggscale change request", body
}

func strPtr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
