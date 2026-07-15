// Package quota holds the per-class limit ladder for enforced tenants and the
// growth checks that back it. Limits apply only when a tenant has
// enforce_quotas=true; zero-config self-host stays uncapped. Numbered classes
// carry no price judgment — the ladder is deliberately generous, and projects,
// registered players, and object storage are the upgrade levers (CCU is not).
// See docs/temp/tier-rework.md.
package quota

import (
	"fmt"

	"github.com/ggscale/ggscale/internal/tenant"
)

// Unlimited is the sentinel for a class axis with no cap (tier_3 projects and
// players). A Check on an Unlimited axis always passes.
const Unlimited = -1

// Axis labels identify which quota a rejection hit. Kept low-cardinality for
// the rejection metric.
const (
	AxisProjects = "projects"
	AxisPlayers  = "players"
	AxisStorage  = "storage"
)

const gb = int64(1) << 30

// Limits is the per-class quota ladder. Projects is a small count; Players and
// StorageBytes are int64. Unlimited (-1) marks an uncapped axis.
type Limits struct {
	Projects     int
	Players      int64
	StorageBytes int64
}

// LimitsForClass returns the quota ladder for a tenant class. Unknown/out-of-
// range classes fall back to tier_0 — fail-closed, matching the rate ladder.
func LimitsForClass(t tenant.Tier) Limits {
	switch t {
	case tenant.Tier1:
		return Limits{Projects: 10, Players: 1_000_000, StorageBytes: 25 * gb}
	case tenant.Tier2:
		return Limits{Projects: 20, Players: 5_000_000, StorageBytes: 100 * gb}
	case tenant.Tier3:
		return Limits{Projects: Unlimited, Players: Unlimited, StorageBytes: 500 * gb}
	default:
		return Limits{Projects: 3, Players: 250_000, StorageBytes: 5 * gb}
	}
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
