// Package entitlement serves the internal declarative entitlement API an
// external billing service calls to converge a tenant onto "tier N with
// features […]". It reuses the same tier UPDATE, feature_grants
// upsert/disable, audit writer, and admin notification emails as the human
// change-request approve path, so billing-driven and human changes behave
// identically; audit rows stay distinguishable through their
// "billing-service" actor. The surface is deliberately outside /v1 (and
// openapi.yaml): production deploys bind/firewall it to a private network.
package entitlement

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/ggscale/ggscale/internal/auditlog"
	"github.com/ggscale/ggscale/internal/db"
	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/mailer"
	"github.com/ggscale/ggscale/internal/observability"
	"github.com/ggscale/ggscale/internal/tenant"
)

// managedFeatures are the tenant-level features the billing service may grant
// and revoke — the same umbrella features a tenant can request through the
// change-request workflow. Grants outside this set (e.g. per-backend fleet
// features, human custom deals) are never touched by a declarative apply.
var managedFeatures = []string{"p2p_relay", "dedicated_servers"}

const (
	auditEntitlementApply = "entitlement.apply"
	auditActor            = "billing-service"
	grantReason           = "billing entitlement"
)

// State is the declarative entitlement shape: what the tenant should have,
// not what to change.
type State struct {
	Tier     int      `json:"tier"`
	Features []string `json:"features"`
}

// Deps groups what New needs to serve the API.
type Deps struct {
	Pool     *db.Pool
	Mailer   mailer.Mailer
	MailFrom string
	// Token is the static bearer credential (from LoadToken).
	Token   string
	Metrics *observability.Metrics
	// ReloadRBAC refreshes the casbin policy after a grant change (feature
	// grants feed authorization). nil is a no-op.
	ReloadRBAC func(ctx context.Context)
}

// New builds the entitlement API handler: GET and PUT /{tenantID}, both
// behind the bearer token.
func New(d Deps) http.Handler {
	h := &handler{d: d}
	r := chi.NewRouter()
	r.Use(requireBearer(d.Token))
	r.Get("/{tenantID}", h.getEntitlements)
	r.Put("/{tenantID}", h.putEntitlements)
	return r
}

type handler struct {
	d Deps
}

// requireBearer gates every route behind the static token. Both sides are
// SHA-256'd before the constant-time compare so the comparison always runs
// over equal-length digests (same idiom as the /metrics token guard).
func requireBearer(token string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented, ok := bearerCredential(r.Header.Get("Authorization"))
			got := sha256.Sum256([]byte(presented))
			if token == "" || !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerCredential(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

func validateState(s State) error {
	if s.Tier < int(tenant.Tier0) || s.Tier > int(tenant.Tier3) {
		return fmt.Errorf("tier %d is outside the 0..%d class ladder", s.Tier, int(tenant.Tier3))
	}
	for _, f := range s.Features {
		if !slices.Contains(managedFeatures, f) {
			return fmt.Errorf("feature %q is not billing-manageable", f)
		}
	}
	return nil
}

// stateDiff is what a declarative apply must change to reach the desired
// state. Empty on all axes means the apply is a no-op.
type stateDiff struct {
	oldTier, newTier int
	enable, disable  []string
}

func (d stateDiff) changed() bool {
	return d.oldTier != d.newTier || len(d.enable) > 0 || len(d.disable) > 0
}

// diffStates compares the tenant's current tier + enabled features against
// the desired state. Only managed features are considered on either side, so
// human custom grants survive billing applies untouched.
func diffStates(currentTier int, currentFeatures []string, desired State) stateDiff {
	d := stateDiff{oldTier: currentTier, newTier: desired.Tier}
	for _, f := range managedFeatures {
		has := slices.Contains(currentFeatures, f)
		wants := slices.Contains(desired.Features, f)
		switch {
		case wants && !has:
			d.enable = append(d.enable, f)
		case has && !wants:
			d.disable = append(d.disable, f)
		}
	}
	return d
}

var errTenantNotFound = errors.New("tenant not found")

func (h *handler) getEntitlements(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parseTenantID(w, r)
	if !ok {
		return
	}
	state, unmanaged, err := h.loadState(r.Context(), tenantID)
	if errors.Is(err, errTenantNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "entitlement: load", "err", err, "tenant_id", tenantID)
		http.Error(w, "entitlement load failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, getResponse{State: state, UnmanagedFeatures: unmanaged})
}

// getResponse is the reconcile read side: the billing-managed state plus the
// tenant's other enabled grants (human deals, fleet backends), reported
// separately so the reconcile never counts them as drift.
type getResponse struct {
	State
	UnmanagedFeatures []string `json:"unmanaged_features"`
}

type applyResponse struct {
	Changed bool `json:"changed"`
	State
}

func (h *handler) putEntitlements(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := parseTenantID(w, r)
	if !ok {
		h.d.Metrics.EntitlementApply(observability.EntitlementRejected)
		return
	}
	var desired State
	if err := json.NewDecoder(r.Body).Decode(&desired); err != nil {
		h.d.Metrics.EntitlementApply(observability.EntitlementRejected)
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := validateState(desired); err != nil {
		h.d.Metrics.EntitlementApply(observability.EntitlementRejected)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	diff, err := h.apply(r.Context(), tenantID, desired)
	if errors.Is(err, errTenantNotFound) {
		h.d.Metrics.EntitlementApply(observability.EntitlementRejected)
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "entitlement: apply", "err", err, "tenant_id", tenantID)
		http.Error(w, "entitlement apply failed", http.StatusInternalServerError)
		return
	}

	if !diff.changed() {
		h.d.Metrics.EntitlementApply(observability.EntitlementNoOp)
		writeJSON(w, applyResponse{Changed: false, State: canonicalState(desired)})
		return
	}
	h.d.Metrics.EntitlementApply(observability.EntitlementChanged)
	if h.d.ReloadRBAC != nil {
		h.d.ReloadRBAC(r.Context())
	}
	h.sendChangeEmail(r.Context(), tenantID, diff)
	writeJSON(w, applyResponse{Changed: true, State: canonicalState(desired)})
}

// apply converges the tenant onto desired in one tenant-context transaction:
// the diff read, the tier UPDATE, the grant upserts/disables, and the single
// audit row commit or roll back together.
func (h *handler) apply(ctx context.Context, tenantID int64, desired State) (stateDiff, error) {
	var diff stateDiff
	tctx := db.WithTenant(ctx, tenantID)
	err := h.d.Pool.Q(tctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		current, _, err := loadStateTx(tctx, q, tenantID)
		if err != nil {
			return err
		}
		diff = diffStates(current.Tier, current.Features, desired)
		if !diff.changed() {
			return nil
		}

		if diff.oldTier != diff.newTier {
			//nolint:gosec // bounded to 0..3 by validateState
			if _, err := q.SetTenantTierExact(tctx, int16(diff.newTier)); err != nil {
				return err
			}
		}
		reason := grantReason
		for _, f := range diff.enable {
			if err := q.UpsertTenantFeatureGrant(tctx, sqlcgen.UpsertTenantFeatureGrantParams{
				Feature: f,
				Reason:  &reason,
			}); err != nil {
				return err
			}
		}
		for _, f := range diff.disable {
			if _, err := q.DisableTenantFeatureGrant(tctx, sqlcgen.DisableTenantFeatureGrantParams{
				Feature: f,
				Reason:  &reason,
			}); err != nil {
				return err
			}
		}
		return auditlog.WritePlatformService(tctx, tx, auditActor, auditEntitlementApply,
			strconv.FormatInt(tenantID, 10), map[string]any{
				"tenant_id":         tenantID,
				"old_tier":          diff.oldTier,
				"new_tier":          diff.newTier,
				"features_enabled":  diff.enable,
				"features_disabled": diff.disable,
			})
	})
	return diff, err
}

func (h *handler) loadState(ctx context.Context, tenantID int64) (State, []string, error) {
	var state State
	var unmanaged []string
	tctx := db.WithTenant(ctx, tenantID)
	err := h.d.Pool.Q(tctx, func(tx pgx.Tx) error {
		var e error
		state, unmanaged, e = loadStateTx(tctx, sqlcgen.New(tx), tenantID)
		return e
	})
	return state, unmanaged, err
}

// loadStateTx reads the tenant's current entitlement state inside an existing
// tenant-context transaction, split into billing-managed features and the
// rest.
func loadStateTx(tctx context.Context, q *sqlcgen.Queries, tenantID int64) (State, []string, error) {
	facts, err := q.GetTenantFacts(tctx, tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, nil, errTenantNotFound
	}
	if err != nil {
		return State{}, nil, err
	}
	enabled, err := q.ListTenantEnabledFeatures(tctx)
	if err != nil {
		return State{}, nil, err
	}
	state := State{Tier: int(tenant.ClampTier(int(facts.Tier))), Features: []string{}}
	unmanaged := []string{}
	for _, f := range enabled {
		if slices.Contains(managedFeatures, f) {
			state.Features = append(state.Features, f)
		} else {
			unmanaged = append(unmanaged, f)
		}
	}
	slices.Sort(state.Features)
	slices.Sort(unmanaged)
	return state, unmanaged, nil
}

// canonicalState normalizes the echoed desired state (deduped, sorted,
// never-nil features) so PUT responses match what a follow-up GET returns.
func canonicalState(s State) State {
	out := State{Tier: s.Tier, Features: []string{}}
	for _, f := range managedFeatures {
		if slices.Contains(s.Features, f) {
			out.Features = append(out.Features, f)
		}
	}
	slices.Sort(out.Features)
	return out
}

// sendChangeEmail notifies the tenant's owner/admins that their plan changed,
// mirroring the change-request decision emails (plan- and price-free copy).
func (h *handler) sendChangeEmail(ctx context.Context, tenantID int64, diff stateDiff) {
	if h.d.Mailer == nil || h.d.MailFrom == "" {
		return
	}
	var emails []string
	if err := h.d.Pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var e error
		emails, e = sqlcgen.New(tx).ListTenantAdminEmails(ctx, tenantID)
		return e
	}); err != nil {
		slog.ErrorContext(ctx, "entitlement: list admin emails", "err", err, "tenant_id", tenantID)
		return
	}
	if len(emails) == 0 {
		return
	}
	subject, body := changeEmail(diff)
	if err := h.d.Mailer.Send(ctx, mailer.Message{From: h.d.MailFrom, To: emails, Subject: subject, Body: body}); err != nil {
		slog.ErrorContext(ctx, "entitlement: change mailer", "err", err, "tenant_id", tenantID)
	}
}

func changeEmail(diff stateDiff) (subject, body string) {
	var lines []string
	if diff.oldTier != diff.newTier {
		lines = append(lines, fmt.Sprintf("Your tenant's service class is now %s.", tenant.Tier(diff.newTier).String()))
	}
	if len(diff.enable) > 0 {
		lines = append(lines, "Features enabled: "+strings.Join(diff.enable, ", ")+".")
	}
	if len(diff.disable) > 0 {
		lines = append(lines, "Features disabled: "+strings.Join(diff.disable, ", ")+".")
	}
	return "Your ggscale plan was updated", strings.Join(lines, "\n")
}

func parseTenantID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "tenantID"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("entitlement: encode response", "err", err)
	}
}
