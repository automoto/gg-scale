package controlpanel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/gamesession"
	"github.com/automoto/gg-scale/internal/mailer"
	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/tenant"
	"github.com/automoto/gg-scale/internal/webutil"
)

const (
	pathAdminChangeRequests   = pathControlPanel + "/admin/change-requests"
	auditChangeRequestApprove = "control_panel.change_request.approve"
	auditChangeRequestDeny    = "control_panel.change_request.deny"

	changeKindTierUpgrade   = "tier_upgrade"
	changeKindFeature       = "feature"
	changeKindQuotaOverride = "quota_override"
)

// errNotAnUpgrade rejects a tier-upgrade request whose target class is not
// strictly above the tenant's current class. The submit form only offers
// above-current classes, but a crafted POST can request any 0..3 value; without
// this guard an approval would silently downgrade the tenant's quotas/caps.
var errNotAnUpgrade = errors.New("requested tier is not above the current tier")

var errFeatureAlreadyEnabled = errors.New("requested feature is already enabled")

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

// quotaOverrideAxes are the per-tenant quota axes a tenant may request an
// override for (and a platform admin may set directly). The Value strings are
// the internal/quota Axis* constants, carried in the change request's feature
// column and in tenant_quota_overrides.axis.
var quotaOverrideAxes = []struct{ Value, Label string }{
	{quota.AxisProjects, "Game Projects"},
	{quota.AxisPlayers, "Registered players"},
	{quota.AxisStorage, "Storage (bytes)"},
	{quota.AxisRelaySessions, "Relay sessions per month"},
	{quota.AxisOpenSessions, "Open game sessions per Game Project"},
}

// adminOnlyQuotaAxes are quota axes only a platform admin may set. Tenants
// cannot request them, and the change-request feature CHECK refuses them too.
var adminOnlyQuotaAxes = []struct{ Value, Label string }{
	{quota.AxisRelayAllocations, "Relay allocations per relay node"},
	{quota.AxisLeaderboards, "Leaderboards per Game Project"},
}

// errNotRequestableAxis refuses a change request for an axis tenants may not
// request.
var errNotRequestableAxis = errors.New("quota axis cannot be requested by a tenant")

// isQuotaAxis reports whether a tenant may request an override for axis.
func isQuotaAxis(axis string) bool {
	for _, a := range quotaOverrideAxes {
		if a.Value == axis {
			return true
		}
	}
	return false
}

// isAdminQuotaAxis reports whether a platform admin may set an override for
// axis.
func isAdminQuotaAxis(axis string) bool {
	return isQuotaAxis(axis) || slices.ContainsFunc(adminOnlyQuotaAxes, func(a struct{ Value, Label string }) bool { return a.Value == axis })
}

func quotaAxisLabel(axis string) string {
	for _, a := range slices.Concat(quotaOverrideAxes, adminOnlyQuotaAxes) {
		if a.Value == axis {
			return a.Label
		}
	}
	return axis
}

// parseRequestedLimit parses a quota-override limit: a non-negative count, or
// -1 for unlimited.
func parseRequestedLimit(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < -1 {
		return 0, errors.New("invalid limit")
	}
	return n, nil
}

// errOverrideAboveHardCap rejects an open_sessions override the service can
// never honor: session creation stops at gamesession.SessionsHardCap for
// every tenant, so an override above it (or "unlimited") would overpromise.
var errOverrideAboveHardCap = fmt.Errorf(
	"open_sessions override exceeds the absolute per-project cap (%d)", gamesession.SessionsHardCap)

// errLeaderboardsUnlimited rejects an unlimited leaderboards override: the
// limit keeps player discovery bounded, so it always needs a number.
var errLeaderboardsUnlimited = errors.New("leaderboards override must be a number, not unlimited")

// validateOverrideLimit applies per-axis bounds on top of parseRequestedLimit.
func validateOverrideLimit(axis string, limit int64) error {
	if axis == quota.AxisLeaderboards && limit == quota.Unlimited {
		return errLeaderboardsUnlimited
	}
	if axis != quota.AxisOpenSessions {
		return nil
	}
	if limit == quota.Unlimited || limit > gamesession.SessionsHardCap {
		return errOverrideAboveHardCap
	}
	return nil
}

// quotaOverrideDetail renders "axis → limit" for request lists and emails.
func quotaOverrideDetail(axis *string, limit *int64) string {
	if axis == nil || limit == nil {
		return ""
	}
	value := strconv.FormatInt(*limit, 10)
	if *limit == quota.Unlimited {
		value = "unlimited"
	}
	return quotaAxisLabel(*axis) + " → " + value
}

// featureEnabledByEnv reports whether a requestable feature's server-side kill
// switch is on, so the tenant isn't offered a feature the server can't serve.
func (h *Handler) featureEnabledByEnv(feature string) bool {
	on, _ := h.featureSwitch(feature)
	return on
}

// featureSwitch returns the state of the server switch of a feature, and
// whether the feature has a switch. A feature with no switch (only a grant)
// is not on.
func (h *Handler) featureSwitch(feature string) (on, known bool) {
	switch feature {
	case "p2p_relay":
		return h.cfg.RelayEnabled, true
	case "dedicated_servers", "fleet_agones_backend", "fleet_plugin_backend":
		return h.cfg.FleetEnabled, true
	default:
		return false, false
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

		// A platform admin sees every grantable feature with its current state,
		// including ones already held (to revoke) or the server can't serve.
		for _, f := range requestableFeatures {
			_, held := heldSet[f.Value]
			view.AdminFeatureGrants = append(view.AdminFeatureGrants, AdminFeatureGrantView{
				Value:      f.Value,
				Label:      f.Label,
				Enabled:    held,
				EnvAllowed: h.featureEnabledByEnv(f.Value),
			})
		}

		for _, a := range quotaOverrideAxes {
			view.QuotaAxisOptions = append(view.QuotaAxisOptions, FeatureOptionView{Value: a.Value, Label: a.Label})
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
	case row.Kind == changeKindQuotaOverride:
		v.Detail = quotaOverrideDetail(row.Feature, row.RequestedLimit)
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
			h.redirectTenantSettings(w, r, tenantID, flashRequestClassInvalid)
			return
		}
		params.RequestedTier = &tierNum
	case changeKindFeature:
		feature := r.Form.Get("feature")
		if !h.isRequestableFeature(feature) {
			h.redirectTenantSettings(w, r, tenantID, flashRequestFeatureInvalid)
			return
		}
		params.Feature = &feature
	case changeKindQuotaOverride:
		axis := r.Form.Get("axis")
		if !isQuotaAxis(axis) {
			h.redirectTenantSettings(w, r, tenantID, flashRequestQuotaInvalid)
			return
		}
		limit, perr := parseRequestedLimit(r.Form.Get("requested_limit"))
		if perr != nil {
			h.redirectTenantSettings(w, r, tenantID, flashRequestLimitInvalid)
			return
		}
		if verr := validateOverrideLimit(axis, limit); verr != nil {
			h.redirectTenantSettings(w, r, tenantID, flashSessionsAboveCap)
			return
		}
		params.Feature = &axis
		params.RequestedLimit = &limit
	default:
		h.redirectTenantSettings(w, r, tenantID, flashRequestTypeUnknown)
		return
	}

	tctx := db.WithTenant(r.Context(), tenantID)
	err := h.pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		switch kind {
		case changeKindTierUpgrade:
			facts, ferr := q.GetTenantFacts(r.Context(), tenantID)
			if ferr != nil {
				return ferr
			}
			if !tierIsUpgrade(*params.RequestedTier, facts.Tier) {
				return errNotAnUpgrade
			}
		case changeKindFeature:
			held, ferr := q.ListTenantEnabledFeatures(tctx)
			if ferr != nil {
				return ferr
			}
			for _, feature := range held {
				if feature == *params.Feature {
					return errFeatureAlreadyEnabled
				}
			}
		}
		_, cerr := q.CreateTenantChangeRequest(tctx, params)
		return cerr
	})
	if errors.Is(err, errNotAnUpgrade) {
		h.redirectTenantSettings(w, r, tenantID, flashRequestClassNotAbove)
		return
	}
	if errors.Is(err, errFeatureAlreadyEnabled) {
		h.redirectTenantSettings(w, r, tenantID, flashRequestFeatureEnabled)
		return
	}
	if isUniqueViolation(err) {
		h.redirectTenantSettings(w, r, tenantID, flashRequestPending)
		return
	}
	if err != nil {
		webutil.InternalError(w, "change request: create", err)
		return
	}
	h.redirectTenantSettings(w, r, tenantID, flashRequestSubmitted)
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

func (h *Handler) redirectTenantSettings(w http.ResponseWriter, r *http.Request, tenantID int64, code flashCode) {
	htmxRedirect(w, r, withFlash(pathTenantsPrefix+strconv.FormatInt(tenantID, 10)+"/settings", code))
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
		v := pendingChangeRequestView(row)
		v.Warning = agonesGrantWarning(v, h.cfg.ServerSettings.AgonesProjectIsolation)
		reqs = append(reqs, v)
	}
	webutil.Render(r, w, ChangeRequestsPage(ChangeRequestsView{
		UserEmail: session.User.Email,
		CSRFToken: session.CSRFToken,
		Requests:  reqs,
		Message:   flashMessage(r),
	}))
}

// agonesIsolationOffWarning is shown where a platform admin can give the
// fleet_agones_backend grant, and on the Server settings page, while
// AGONES_PROJECT_ISOLATION is off.
const agonesIsolationOffWarning = "Agones project isolation is off. Every Game Project with the fleet_agones_backend grant can allocate and delete the GameServers of the other Game Projects on this Agones cluster. Give that grant to one Game Project only, or set AGONES_PROJECT_ISOLATION=true and label the GameServer template of each Agones Fleet with ggscale.dev/project=<project id>."

// agonesGrantWarning returns the warning for a request of the
// fleet_agones_backend grant while Agones project isolation is off. The
// submit form does not offer that grant, but approve applies an older
// request that holds it.
func agonesGrantWarning(v PendingChangeRequestView, isolation bool) string {
	if isolation || v.Kind != changeKindFeature || v.Target != "fleet_agones_backend" {
		return ""
	}
	return agonesIsolationOffWarning
}

func pendingChangeRequestView(row sqlcgen.ListPendingTenantChangeRequestsRow) PendingChangeRequestView {
	v := PendingChangeRequestView{
		ID:          row.ID,
		TenantID:    row.TenantID,
		TenantName:  row.TenantName,
		CurrentTier: tenant.ClampTier(int(row.CurrentTier)).String(),
		Kind:        row.Kind,
		Note:        row.Note,
		Requester:   row.RequesterEmail,
		CreatedAt:   row.CreatedAt.Time,
	}
	switch {
	case row.Kind == changeKindTierUpgrade && row.RequestedTier != nil:
		v.Target = tenant.ClampTier(int(*row.RequestedTier)).String()
	case row.Kind == changeKindQuotaOverride:
		v.Target = quotaOverrideDetail(row.Feature, row.RequestedLimit)
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
		h.redirectChangeAdmin(w, r, flashRequestHandled)
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
			h.redirectChangeAdmin(w, r, flashRequestClassNotApproved)
			return
		}
		if errors.Is(err, errOverrideAboveHardCap) {
			h.redirectChangeAdmin(w, r, flashRequestCapNotApproved)
			return
		}
		webutil.InternalError(w, "change request: approve", err)
		return
	}
	if !handled {
		h.redirectChangeAdmin(w, r, flashRequestHandled)
		return
	}
	// Drop first: the email can be slow, and the change is already committed.
	h.dropTenantCaches(applied.TenantID)
	h.sendChangeRequestDecisionEmail(r.Context(), applied, true)
	h.redirectChangeAdmin(w, r, flashRequestApproved)
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
	case req.Kind == changeKindQuotaOverride && req.Feature != nil && req.RequestedLimit != nil:
		if !isQuotaAxis(*req.Feature) {
			return errNotRequestableAxis
		}
		// Re-validate at apply time: submit-side validation can't cover a
		// request that predates a bounds change.
		if err := validateOverrideLimit(*req.Feature, *req.RequestedLimit); err != nil {
			return err
		}
		return q.UpsertQuotaOverride(ctx, sqlcgen.UpsertQuotaOverrideParams{
			TenantID:   req.TenantID,
			Axis:       *req.Feature,
			LimitValue: *req.RequestedLimit,
			UpdatedBy:  &actorID,
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
		h.redirectChangeAdmin(w, r, flashRequestHandled)
		return
	}
	h.sendChangeRequestDecisionEmail(r.Context(), req, false)
	h.redirectChangeAdmin(w, r, flashRequestDenied)
}

func (h *Handler) redirectChangeAdmin(w http.ResponseWriter, r *http.Request, code flashCode) {
	htmxRedirect(w, r, withFlash(pathAdminChangeRequests, code))
}

// sendChangeRequestDecisionEmail notifies the tenant's owner/admins of an
// approve/deny outcome. Mirrors the signup decision emails.
func (h *Handler) sendChangeRequestDecisionEmail(ctx context.Context, req sqlcgen.GetTenantChangeRequestByIDRow, approved bool) {
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
	subject, body := changeRequestDecisionEmail(req, approved)
	if err := h.mailer.Send(ctx, mailer.Message{From: h.cfg.MailFrom, To: emails, Subject: subject, Body: body}); err != nil {
		slog.ErrorContext(ctx, "change request: decision mailer", "err", err, "tenant_id", req.TenantID)
	}
}

// changeRequestDecisionEmail builds the decision email. The deny reason
// comes from the saved row, so the email shows what was stored.
func changeRequestDecisionEmail(req sqlcgen.GetTenantChangeRequestByIDRow, approved bool) (subject, body string) {
	what := "feature " + deref(req.Feature)
	switch {
	case req.Kind == changeKindTierUpgrade && req.RequestedTier != nil:
		what = "upgrade to " + tenant.ClampTier(int(*req.RequestedTier)).String()
	case req.Kind == changeKindQuotaOverride:
		what = "quota override " + quotaOverrideDetail(req.Feature, req.RequestedLimit)
	}
	if approved {
		return "Your ggscale change request was approved",
			fmt.Sprintf("Your request (%s) was approved and applied to your Account Tenant.", what)
	}
	body = fmt.Sprintf("Your request (%s) was not approved at this time.", what)
	if reason := deref(req.ReviewReason); reason != "" {
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
