package controlpanel

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/auditlog"
	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/tenant"
)

var (
	errInvalidLimit          = errors.New("control panel: rate and burst must be finite non-negative numbers")
	errExceedsCap            = errors.New("control panel: per-project quota exceeds the tenant cap")
	errIncompleteLimit       = errors.New("control panel: rate and burst must both be positive (or both blank to clear)")
	errConnectionCeiling     = errors.New("control panel: temporary connection maximum must be at least the sustained limit")
	errConnectionBurstRatio  = errors.New("control panel: temporary connection maximum must not exceed twice the sustained limit")
	errConnectionAbsoluteMax = errors.New("control panel: connection limit exceeds the platform safety maximum")
	errConnectionEnvOverride = errors.New("control panel: REALTIME_MAX_PER_TENANT supersedes tenant connection overrides")
)

// finiteNonNegative reports whether every value is a real number >= 0. It
// rejects NaN/Inf, which otherwise slip past bound checks (all comparisons
// against NaN are false) and disable the limit.
func finiteNonNegative(vs ...float64) bool {
	for _, v := range vs {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// rateLimitsView assembles the tenant HTTP API override (platform-admin) plus
// per-project invite quotas (tenant-admin). Absent overrides render as the
// compiled defaults so the operator sees what they're changing from.
func (h *Handler) rateLimitsView(ctx context.Context, tenantID int64) (RateLimitsView, error) {
	view := RateLimitsView{
		TenantID:                     tenantID,
		ConnectionEnvMax:             h.connectionEnvMax,
		DefaultInviterHour:           ratelimit.DefaultInviteLimits.InviterPerHour,
		DefaultDomainDay:             ratelimit.DefaultInviteLimits.DomainPerDay,
		DefaultRecipientBurst:        ratelimit.DefaultInviteLimits.RecipientBurst,
		DefaultRecipientCooldownSecs: ratelimit.DefaultInviteLimits.RecipientCooldown.Seconds(),
	}

	projects, err := h.listProjects(ctx, tenantID)
	if err != nil {
		return RateLimitsView{}, err
	}

	// Per-project invite rows are grouped by project id from a single
	// tenant-wide query (no per-project round trip).
	perProject := map[int64]*ProjectInviteLimitView{}
	ordered := make([]*ProjectInviteLimitView, 0, len(projects))
	for _, p := range projects {
		pv := &ProjectInviteLimitView{ProjectID: p.ID, ProjectName: p.Name}
		perProject[p.ID] = pv
		ordered = append(ordered, pv)
	}

	err = h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		// Show the compiled default for the tenant's actual tier — enforcement
		// keys off the same tier, so a hardcoded free-tier default would mislead
		// a paid tenant about what clearing the override restores.
		facts, err := q.GetTenantFacts(ctx, tenantID)
		if err != nil {
			return err
		}
		view.TenantName = facts.Name
		clamped := tenant.ClampTier(int(facts.Tier))
		defaults := ratelimit.LimitsForTier(clamped)
		view.APIDefaultRate = defaults.RatePerSecond
		view.APIDefaultBurst = defaults.Burst
		connectionDefaults := ratelimit.ConnectionCapForClass(clamped)
		view.ConnectionDefaultSustained = connectionDefaults.Sustained
		view.ConnectionDefaultCeiling = connectionDefaults.Ceiling
		connectionOverride, err := q.GetConnectionLimitOverride(ctx, tenantID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			view.ConnectionOverridden = true
			view.ConnectionSustained = connectionOverride.Sustained
			view.ConnectionCeiling = connectionOverride.Ceiling
		}
		overrideRows, err := q.ListQuotaOverridesForTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		overrides := make(map[string]int64, len(overrideRows))
		for _, row := range overrideRows {
			overrides[row.Axis] = row.Limit
		}
		view.StorageTotalBytes = quota.Resolve(clamped, overrides).StorageBytes
		view.QuotaOverrides = buildQuotaOverrideRows(clamped, overrides)

		rows, err := q.ListAllRateLimitOverridesForTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.ProjectID == nil {
				switch row.Kind {
				case ratelimit.OverrideKindAPI:
					view.APIOverridden = true
					view.APIRate = row.Rate
					view.APIBurst = row.Burst
				case ratelimit.OverrideKindInviteRecipient:
					view.RecipientOverridden = true
					view.RecipientBurst = row.Burst
					view.RecipientCooldownSecs = cooldownSecsFromRate(row.Rate)
				}
				continue
			}
			pv, ok := perProject[*row.ProjectID]
			if !ok {
				continue // override for a deleted/foreign project — skip
			}
			switch row.Kind {
			case ratelimit.OverrideKindInviteInviter:
				pv.InviterPerHour = row.Burst
			case ratelimit.OverrideKindInviteDomain:
				pv.DomainPerDay = row.Burst
			}
		}
		return nil
	})
	if err != nil {
		return RateLimitsView{}, err
	}

	// Storage value-size limits: platform default plus tenant/project overrides.
	view.StoragePlatformDefault = h.storagePlatformDefault()
	if h.storageLimits != nil {
		overrides, serr := h.storageLimits.ListForTenant(ctx, tenantID)
		if serr != nil {
			return RateLimitsView{}, serr
		}
		for _, o := range overrides {
			if o.ProjectID == nil {
				view.StorageTenantOverride = o.MaxValueBytes
				continue
			}
			if pv, ok := perProject[*o.ProjectID]; ok {
				pv.StorageOverrideBytes = o.MaxValueBytes
			}
		}
	}

	for _, pv := range ordered {
		view.Projects = append(view.Projects, *pv)
	}
	return view, nil
}

// setTenantConnectionOverride writes a tenant-specific realtime admission
// envelope, or clears it when both values are zero. Platform admins use this
// for contracted launch capacity and breakout traffic without changing the
// defaults for every tenant in the billing class.
func (h *Handler) setTenantConnectionOverride(ctx context.Context, actorID, tenantID, sustained, ceiling int64) error {
	if h.connectionEnvMax > 0 {
		return errConnectionEnvOverride
	}
	if sustained < 0 || ceiling < 0 {
		return errInvalidLimit
	}
	if (sustained == 0) != (ceiling == 0) {
		return errIncompleteLimit
	}
	if ceiling < sustained {
		return errConnectionCeiling
	}
	if sustained > ratelimit.MaxCustomConnectionLimit || ceiling > ratelimit.MaxCustomConnectionLimit {
		return errConnectionAbsoluteMax
	}
	if ceiling > 2*sustained {
		return errConnectionBurstRatio
	}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if sustained == 0 {
			rows, err := q.DeleteConnectionLimitOverride(ctx, tenantID)
			if err != nil {
				return err
			}
			if rows == 0 {
				return nil
			}
			return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.connection_limit.clear",
				strconv.FormatInt(tenantID, 10), map[string]any{"tenant_id": tenantID})
		}
		if err := q.UpsertConnectionLimitOverride(ctx, sqlcgen.UpsertConnectionLimitOverrideParams{
			TenantID: tenantID, Sustained: sustained, Ceiling: ceiling, UpdatedBy: &actorID,
		}); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.connection_limit.set",
			strconv.FormatInt(tenantID, 10), map[string]any{
				"tenant_id": tenantID, "sustained": sustained, "ceiling": ceiling,
			})
	})
	if err != nil {
		return err
	}
	h.invalidateConnectionLimits(tenantID)
	return nil
}

// setTenantAPIOverride writes (or, when both values are zero, clears) the
// tenant-wide HTTP API rate limit. Platform-admin only.
func (h *Handler) setTenantAPIOverride(ctx context.Context, actorID, tenantID int64, rate, burst float64) error {
	if !finiteNonNegative(rate, burst) {
		return errInvalidLimit
	}
	// Both-zero clears the override. A one-sided zero would persist a bucket
	// that never allows traffic — a zero rate never refills, a zero burst has
	// no capacity — pinning every key for the tenant at a permanent 429.
	if (rate == 0) != (burst == 0) {
		return errIncompleteLimit
	}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if rate == 0 && burst == 0 {
			if err := q.DeleteRateLimitOverride(ctx, sqlcgen.DeleteRateLimitOverrideParams{
				TenantID: tenantID, Kind: ratelimit.OverrideKindAPI, ProjectID: nil,
			}); err != nil {
				return err
			}
			return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.rate_limit.clear", strconv.FormatInt(tenantID, 10),
				map[string]any{"kind": ratelimit.OverrideKindAPI, "tenant_id": tenantID})
		}
		if err := q.UpsertRateLimitOverride(ctx, sqlcgen.UpsertRateLimitOverrideParams{
			TenantID: tenantID, ProjectID: nil, Kind: ratelimit.OverrideKindAPI,
			Rate: rate, Burst: burst, UpdatedBy: &actorID,
		}); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.rate_limit.set", strconv.FormatInt(tenantID, 10),
			map[string]any{"kind": ratelimit.OverrideKindAPI, "tenant_id": tenantID, "rate": rate, "burst": burst})
	})
	if err != nil {
		return err
	}
	h.invalidateOverrides(tenantID)
	return nil
}

// cooldownSecsFromRate reverse-derives a per-recipient cooldown (seconds) from a
// stored token-bucket refill rate for display, rounding away float noise.
func cooldownSecsFromRate(rate float64) float64 {
	if rate <= 0 {
		return 0
	}
	return math.Round((1.0/rate)*1000) / 1000
}

// setTenantRecipientInviteOverride sets (or, when both values are 0, clears) the
// tenant-wide per-recipient invite limit: burst is how many back-to-back invites
// may go to the same address, cooldownSecs the window that gates them. Platform-
// admin only. A one-sided zero is rejected — it would persist a nonsensical
// bucket (zero burst never admits a send; zero cooldown never refills).
func (h *Handler) setTenantRecipientInviteOverride(ctx context.Context, actorID, tenantID int64, burst, cooldownSecs float64) error {
	if !finiteNonNegative(burst, cooldownSecs) {
		return errInvalidLimit
	}
	if (burst == 0) != (cooldownSecs == 0) {
		return errIncompleteLimit
	}
	// Recipient burst is a count of back-to-back sends, so it must be a whole
	// number >= 1. A fractional value (e.g. 1.5) would persist a token-bucket
	// cap that neither matches the displayed integer nor any documented rule.
	if burst != 0 && (burst < 1 || burst != math.Trunc(burst)) {
		return errInvalidLimit
	}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if burst == 0 {
			if err := q.DeleteRateLimitOverride(ctx, sqlcgen.DeleteRateLimitOverrideParams{
				TenantID: tenantID, Kind: ratelimit.OverrideKindInviteRecipient, ProjectID: nil,
			}); err != nil {
				return err
			}
			return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.rate_limit.clear", strconv.FormatInt(tenantID, 10),
				map[string]any{"kind": ratelimit.OverrideKindInviteRecipient, "tenant_id": tenantID})
		}
		if err := q.UpsertRateLimitOverride(ctx, sqlcgen.UpsertRateLimitOverrideParams{
			TenantID: tenantID, ProjectID: nil, Kind: ratelimit.OverrideKindInviteRecipient,
			Rate: 1.0 / cooldownSecs, Burst: burst, UpdatedBy: &actorID,
		}); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.rate_limit.set", strconv.FormatInt(tenantID, 10),
			map[string]any{"kind": ratelimit.OverrideKindInviteRecipient, "tenant_id": tenantID, "burst": burst, "cooldown_secs": cooldownSecs})
	})
	if err != nil {
		return err
	}
	h.invalidateOverrides(tenantID)
	return nil
}

// invalidateOverrides drops the tenant's cached overrides so a just-written
// change takes effect immediately on this process (other cluster nodes converge
// within the cache TTL). No-op when the store doesn't cache.
func (h *Handler) invalidateOverrides(tenantID int64) {
	if inv, ok := h.overrides.(ratelimit.OverrideInvalidator); ok {
		inv.Invalidate(tenantID)
	}
}

func (h *Handler) invalidateConnectionLimits(tenantID int64) {
	if h.connectionLimitInvalidator != nil {
		h.connectionLimitInvalidator.Invalidate(tenantID)
	}
}

// setProjectInviteOverride writes per-project invite quotas from human-facing
// counts (invites/hour, invites/day). A zero clears that kind's override.
func (h *Handler) setProjectInviteOverride(ctx context.Context, actorID, tenantID, projectID int64, inviterPerHour, domainPerDay float64) error {
	if !finiteNonNegative(inviterPerHour, domainPerDay) {
		return errInvalidLimit
	}
	// Per-project quotas are clamped to the tenant cap: a tenant admin can
	// tighten a project below the tenant default but never lift it above.
	// (The compiled default is the tenant cap; there is no tenant-wide invite
	// override to raise it.)
	tenantCap := ratelimit.DefaultInviteLimits
	if inviterPerHour > tenantCap.InviterPerHour || domainPerDay > tenantCap.DomainPerDay {
		return errExceedsCap
	}
	if err := h.requireProjectInTenant(ctx, tenantID, projectID); err != nil {
		return err
	}
	err := h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := upsertOrClearInvite(ctx, q, actorID, tenantID, projectID, ratelimit.OverrideKindInviteInviter, inviterPerHour, 3600); err != nil {
			return err
		}
		if err := upsertOrClearInvite(ctx, q, actorID, tenantID, projectID, ratelimit.OverrideKindInviteDomain, domainPerDay, 86400); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.rate_limit.set", strconv.FormatInt(projectID, 10),
			map[string]any{"tenant_id": tenantID, "project_id": projectID, "inviter_per_hour": inviterPerHour, "domain_per_day": domainPerDay})
	})
	if err != nil {
		return err
	}
	h.invalidateOverrides(tenantID)
	return nil
}

// upsertOrClearInvite maps a per-window count onto a token bucket (burst=count,
// rate=count/windowSecs); count 0 deletes the override so the default applies.
func upsertOrClearInvite(ctx context.Context, q *sqlcgen.Queries, actorID, tenantID, projectID int64, kind string, count, windowSecs float64) error {
	pid := projectID
	if count == 0 {
		return q.DeleteRateLimitOverride(ctx, sqlcgen.DeleteRateLimitOverrideParams{
			TenantID: tenantID, Kind: kind, ProjectID: &pid,
		})
	}
	return q.UpsertRateLimitOverride(ctx, sqlcgen.UpsertRateLimitOverrideParams{
		TenantID: tenantID, ProjectID: &pid, Kind: kind,
		Rate: count / windowSecs, Burst: count, UpdatedBy: &actorID,
	})
}

func (h *Handler) requireProjectInTenant(ctx context.Context, tenantID, projectID int64) error {
	ctx = db.WithTenant(ctx, tenantID)
	return h.pool.Q(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1 AND tenant_id = $2)`,
			projectID, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errProjectNotInTenant
		}
		return nil
	})
}

// rlNum formats a limit for display without trailing zeros.
func rlNum(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// rlValue formats a form field value: blank when unset (0) so the placeholder
// shows the default, otherwise the current override.
func rlValue(f float64) string {
	if f == 0 {
		return ""
	}
	return rlNum(f)
}

func connectionLimitValue(n int64) string {
	if n <= 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func countNum(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if strings.HasPrefix(s, "-") {
		start = 1
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

const (
	bytesPerMiB = int64(1) << 20
	bytesPerGiB = int64(1) << 30
)

// storageMB renders a byte count as megabytes (MiB) without trailing zeros.
func storageMB(n int64) string {
	return strconv.FormatFloat(float64(n)/float64(bytesPerMiB), 'f', -1, 64)
}

// storageGB renders a byte count as gigabytes (GiB) without trailing zeros.
func storageGB(n int64) string {
	return strconv.FormatFloat(float64(n)/float64(bytesPerGiB), 'f', -1, 64)
}

// storageTotalLabel renders the tenant's total storage quota, handling the
// Unlimited sentinel (which storageGB would print as a negative fraction).
func storageTotalLabel(n int64) string {
	if n == quota.Unlimited {
		return "unlimited"
	}
	return storageGB(n) + " GB"
}

// resolvedTenantLimits layers the tenant's quota overrides over its class
// ladder, for the control-panel views (cold path; one extra query in the
// transaction the caller already holds).
func resolvedTenantLimits(ctx context.Context, q *sqlcgen.Queries, tenantID int64, tier tenant.Tier) (quota.Limits, error) {
	rows, err := q.ListQuotaOverridesForTenant(ctx, tenantID)
	if err != nil {
		return quota.Limits{}, err
	}
	overrides := make(map[string]int64, len(rows))
	for _, r := range rows {
		overrides[r.Axis] = r.Limit
	}
	return quota.Resolve(tier, overrides), nil
}

// buildQuotaOverrideRows pairs every quota axis with its ladder default and
// any active override, in the stable quotaOverrideAxes order.
func buildQuotaOverrideRows(tier tenant.Tier, overrides map[string]int64) []QuotaOverrideRowView {
	ladder := quota.LimitsForClass(tier)
	defaults := map[string]int64{
		quota.AxisProjects:      int64(ladder.Projects),
		quota.AxisPlayers:       ladder.Players,
		quota.AxisStorage:       ladder.StorageBytes,
		quota.AxisRelaySessions: ladder.RelaySessionsPerMonth,
		quota.AxisOpenSessions:  ladder.OpenSessionsPerProject,
	}
	rows := make([]QuotaOverrideRowView, 0, len(quotaOverrideAxes))
	for _, a := range quotaOverrideAxes {
		row := QuotaOverrideRowView{Axis: a.Value, Label: a.Label, DefaultLabel: quotaLimitLabel(defaults[a.Value])}
		if v, ok := overrides[a.Value]; ok {
			value := v
			row.Override = &value
		}
		rows = append(rows, row)
	}
	return rows
}

// setQuotaOverride writes (or, with a blank value, clears) one per-tenant
// quota-axis override. Platform-admin only.
func (h *Handler) setQuotaOverride(ctx context.Context, actorID, tenantID int64, axis, limitStr string) error {
	if !isQuotaAxis(axis) {
		return errInvalidLimit
	}
	clear := strings.TrimSpace(limitStr) == ""
	var limit int64
	if !clear {
		var err error
		limit, err = parseRequestedLimit(limitStr)
		if err != nil {
			return errInvalidLimit
		}
		if err := validateOverrideLimit(axis, limit); err != nil {
			return err
		}
	}
	return h.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if clear {
			if err := q.DeleteQuotaOverride(ctx, sqlcgen.DeleteQuotaOverrideParams{
				TenantID: tenantID, Axis: axis,
			}); err != nil {
				return err
			}
			return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.quota_override.clear",
				strconv.FormatInt(tenantID, 10), map[string]any{"tenant_id": tenantID, "axis": axis})
		}
		if err := q.UpsertQuotaOverride(ctx, sqlcgen.UpsertQuotaOverrideParams{
			TenantID: tenantID, Axis: axis, LimitValue: limit, UpdatedBy: &actorID,
		}); err != nil {
			return err
		}
		return auditlog.WritePlatform(ctx, tx, actorID, "control_panel.quota_override.set",
			strconv.FormatInt(tenantID, 10), map[string]any{"tenant_id": tenantID, "axis": axis, "limit": limit})
	})
}

// quotaLimitLabel renders a quota value; the Unlimited sentinel shows as the
// word rather than -1.
func quotaLimitLabel(n int64) string {
	if n == quota.Unlimited {
		return "unlimited"
	}
	return strconv.FormatInt(n, 10)
}

// quotaOverrideValue formats an override form field: blank when unset so the
// placeholder shows the ladder default.
func quotaOverrideValue(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

// storageMBValue formats a storage-limit form field in MB: blank when unset (0)
// so the placeholder shows the platform default, otherwise the override.
func storageMBValue(n int64) string {
	if n <= 0 {
		return ""
	}
	return storageMB(n)
}

func parseLimitField(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: %q", errInvalidLimit, s)
	}
	return v, nil
}

func parseConnectionLimitField(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%w: %q", errInvalidLimit, s)
	}
	return v, nil
}
