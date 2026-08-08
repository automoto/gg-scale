// Package quota holds the per-class limit ladder for enforced tenants and the
// growth checks that back it. Limits apply only when a tenant has
// enforce_quotas=true; zero-config self-host stays uncapped. Numbered classes
// carry no price judgment — the ladder is deliberately generous, and projects,
// registered players, and object storage are the upgrade levers (CCU is not).
// See docs/temp/tier-rework.md.
package quota

import (
	"encoding/json"
	"fmt"

	"github.com/automoto/gg-scale/internal/tenant"
)

// Unlimited is the sentinel for a class axis with no cap (tier_3 projects and
// players). A Check on an Unlimited axis always passes.
const Unlimited = -1

// Axis labels identify which quota a rejection hit. Kept low-cardinality for
// the rejection metric.
const (
	AxisProjects      = "projects"
	AxisPlayers       = "players"
	AxisStorage       = "storage"
	AxisRelaySessions = "relay_sessions"
	AxisOpenSessions  = "open_sessions"
)

const gb = int64(1) << 30

// Limits is the per-class quota ladder. Projects is a small count; Players,
// StorageBytes, and RelaySessionsPerMonth are int64. Unlimited (-1) marks an
// uncapped axis. RelaySessionsPerMonth caps managed-relay credential
// issuances per calendar month; it only bites for tenants holding the
// p2p_relay grant with enforce_quotas on. OpenSessionsPerProject caps live
// game sessions per project; every tenant additionally sits under
// gamesession.SessionsHardCap regardless of enforcement.
type Limits struct {
	Projects               int
	Players                int64
	StorageBytes           int64
	RelaySessionsPerMonth  int64
	OpenSessionsPerProject int64
}

// LimitsForClass returns the quota ladder for a tenant class.
// Unknown/out-of-range classes fall back to tier_0 — fail-closed, matching the rate ladder.
func LimitsForClass(t tenant.Tier) Limits {
	switch t {
	case tenant.Tier1:
		return Limits{Projects: 10, Players: 500_000, StorageBytes: 25 * gb, RelaySessionsPerMonth: 10_000, OpenSessionsPerProject: 2_000}
	case tenant.Tier2:
		return Limits{Projects: 20, Players: 2_000_000, StorageBytes: 100 * gb, RelaySessionsPerMonth: 100_000, OpenSessionsPerProject: 5_000}
	case tenant.Tier3:
		return Limits{Projects: Unlimited, Players: Unlimited, StorageBytes: 500 * gb, RelaySessionsPerMonth: Unlimited, OpenSessionsPerProject: 10_000}
	default:
		return Limits{Projects: 3, Players: 100_000, StorageBytes: 5 * gb, RelaySessionsPerMonth: 1_000, OpenSessionsPerProject: 500}
	}
}

// Resolve layers per-tenant axis overrides (tenant_quota_overrides rows,
// keyed by the Axis* constants) over the class ladder. Unknown axes are
// ignored so an old binary tolerates rows written by a newer one.
func Resolve(t tenant.Tier, overrides map[string]int64) Limits {
	l := LimitsForClass(t)
	for axis, v := range overrides {
		switch axis {
		case AxisProjects:
			l.Projects = int(v)
		case AxisPlayers:
			l.Players = v
		case AxisStorage:
			l.StorageBytes = v
		case AxisRelaySessions:
			l.RelaySessionsPerMonth = v
		case AxisOpenSessions:
			l.OpenSessionsPerProject = v
		}
	}
	return l
}

// ParseOverrides decodes the jsonb axis→limit object the tenant quota-context
// queries aggregate from tenant_quota_overrides. Empty input (no override
// rows) yields a nil map, which Resolve treats as "ladder only".
func ParseOverrides(b []byte) (map[string]int64, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]int64
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("quota overrides: %w", err)
	}
	return m, nil
}

// ResolveSnapshot resolves a tenant's effective limits from the raw tier and
// overrides jsonb carried by the quota-context queries: clamp the class,
// parse the overrides, layer them over the ladder.
func ResolveSnapshot(tier int, overridesJSON []byte) (Limits, error) {
	ov, err := ParseOverrides(overridesJSON)
	if err != nil {
		return Limits{}, err
	}
	return Resolve(tenant.ClampTier(tier), ov), nil
}

// ErrQuotaExceeded is returned by the Check helpers when new growth would cross
// a class limit. It names the axis, the limit, and the current usage so callers
// can render a friendly, upgrade-pointing message.
type ErrQuotaExceeded struct {
	Axis    string
	Limit   int64
	Current int64
}

func (e *ErrQuotaExceeded) Error() string {
	return fmt.Sprintf("quota exceeded: %s (limit %d, current %d)", e.Axis, e.Limit, e.Current)
}

// CheckProjects rejects creating another project when current already meets or
// exceeds the class limit. Existing projects are never affected.
func (l Limits) CheckProjects(current int64) error {
	return checkCount(AxisProjects, int64(l.Projects), current)
}

// CheckPlayers rejects registering another player when current already meets or
// exceeds the class limit. Existing players are never affected.
func (l Limits) CheckPlayers(current int64) error {
	return checkCount(AxisPlayers, l.Players, current)
}

// CheckStorage rejects a growing write (delta > 0) that would push total usage
// past the class limit. Shrinking writes, deletes, and no-ops always pass.
func (l Limits) CheckStorage(current, delta int64) error {
	if delta <= 0 || l.StorageBytes == Unlimited {
		return nil
	}
	if current+delta > l.StorageBytes {
		return &ErrQuotaExceeded{Axis: AxisStorage, Limit: l.StorageBytes, Current: current}
	}
	return nil
}

// CheckOpenSessions rejects creating another game session when the project's
// current open-session count already meets or exceeds the class limit.
// Existing sessions are never affected.
func (l Limits) CheckOpenSessions(current int64) error {
	return checkCount(AxisOpenSessions, l.OpenSessionsPerProject, current)
}

// checkCount is the shared "block new growth at the cap" rule for count axes.
func checkCount(axis string, limit, current int64) error {
	if limit == Unlimited {
		return nil
	}
	if current >= limit {
		return &ErrQuotaExceeded{Axis: axis, Limit: limit, Current: current}
	}
	return nil
}
