package projectadmin

import (
	"context"
	"errors"
	"math"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/db"
	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/storagelimit"
)

// LimitError is a refused limit value. Its text is the message that the
// dashboard and the management API show, so it ends with a period.
type LimitError string

func (e LimitError) Error() string { return string(e) }

// Refused limit values.
const (
	ErrInvalidStorageMB   LimitError = "Storage limit must be a non-negative number (MB)."
	ErrStorageAboveTenant LimitError = "Per Game Project storage limit can't exceed the Account Tenant limit."
	ErrInvalidInviteLimit LimitError = "Values must be finite non-negative numbers."
	ErrInviteAboveCap     LimitError = "Per Game Project invite quota can't exceed the Account Tenant cap."
)

const (
	bytesPerMiB = 1048576

	secondsPerHour = 3600
	secondsPerDay  = 86400

	actionInviteLimitSet  = "control_panel.rate_limit.set"
	actionStorageLimitSet = "control_panel.storage_limit.set"
)

// StorageBytesFromMB converts a limit in MiB to bytes. 0 clears the limit.
func StorageBytesFromMB(mb float64) (int64, error) {
	if mb < 0 || math.IsNaN(mb) || mb > float64(math.MaxInt64/bytesPerMiB) {
		return 0, ErrInvalidStorageMB
	}
	return int64(math.Round(mb * bytesPerMiB)), nil
}

// StorageMBFromBytes returns a byte count in MiB.
func StorageMBFromBytes(n int64) float64 { return float64(n) / bytesPerMiB }

// Limits are the limit settings of one Game Project. A zero value means that
// the project has no override and the default applies.
type Limits struct {
	InviterPerHour float64
	DomainPerDay   float64
	// StorageBytes is the project override of the storage value size.
	StorageBytes int64
	// TenantStorageBytes is the ceiling: the tenant override, or the
	// platform default.
	TenantStorageBytes int64
}

// EffectiveStorageBytes is the value size cap that applies to the project.
func (l Limits) EffectiveStorageBytes() int64 {
	if l.StorageBytes > 0 && l.StorageBytes < l.TenantStorageBytes {
		return l.StorageBytes
	}
	return l.TenantStorageBytes
}

// tenantStorageCeiling returns the tenant storage override, or def.
func tenantStorageCeiling(ctx context.Context, tx pgx.Tx, tenantID, def int64) (int64, error) {
	var v int64
	err := tx.QueryRow(ctx,
		`SELECT max_value_bytes FROM storage_limits WHERE tenant_id = $1 AND project_id IS NULL`, tenantID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// GetLimits returns the limit settings of one project. platformDefault is the
// platform storage value cap.
func GetLimits(ctx context.Context, pool *db.Pool, tenantID, projectID, platformDefault int64) (Limits, error) {
	var out Limits
	err := pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		rows, err := sqlcgen.New(tx).ListAllRateLimitOverridesForTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.ProjectID == nil || *r.ProjectID != projectID {
				continue
			}
			switch r.Kind {
			case ratelimit.OverrideKindInviteInviter:
				out.InviterPerHour = r.Burst
			case ratelimit.OverrideKindInviteDomain:
				out.DomainPerDay = r.Burst
			}
		}
		err = tx.QueryRow(ctx,
			`SELECT max_value_bytes FROM storage_limits WHERE tenant_id = $1 AND project_id = $2`, tenantID, projectID).Scan(&out.StorageBytes)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		out.TenantStorageBytes, err = tenantStorageCeiling(ctx, tx, tenantID, platformDefault)
		return err
	})
	return out, err
}

// SetInviteLimits sets the invite quotas of one project: invites for each
// inviter in an hour, and for the project in a day. 0 clears a quota,
// so the default applies. The limits may not exceed the tenant cap. It
// returns ErrProjectNotInTenant when the project is not in the tenant.
// overrides, when it caches, is told to drop the tenant after the commit.
func SetInviteLimits(ctx context.Context, pool *db.Pool, overrides ratelimit.OverrideStore, tenantID, projectID int64, inviterPerHour, domainPerDay float64, actor Actor) error {
	if !finiteNonNegative(inviterPerHour, domainPerDay) {
		return ErrInvalidInviteLimit
	}
	// The compiled default is the tenant cap: a project may be tighter, not
	// looser. There is no tenant-wide invite override to raise it.
	tenantCap := ratelimit.DefaultInviteLimits
	if inviterPerHour > tenantCap.InviterPerHour || domainPerDay > tenantCap.DomainPerDay {
		return ErrInviteAboveCap
	}
	err := pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		if err := lockProject(ctx, q, projectID); err != nil {
			return err
		}
		if err := setInviteOverride(ctx, q, actor, tenantID, projectID, ratelimit.OverrideKindInviteInviter, inviterPerHour, secondsPerHour); err != nil {
			return err
		}
		if err := setInviteOverride(ctx, q, actor, tenantID, projectID, ratelimit.OverrideKindInviteDomain, domainPerDay, secondsPerDay); err != nil {
			return err
		}
		return actor.Audit(ctx, tx, tenantID, projectID, actionInviteLimitSet, strconv.FormatInt(projectID, 10),
			map[string]any{"inviter_per_hour": inviterPerHour, "domain_per_day": domainPerDay})
	})
	if err != nil {
		return err
	}
	ratelimit.InvalidateOverrides(ctx, overrides, tenantID)
	return nil
}

func finiteNonNegative(vs ...float64) bool {
	for _, v := range vs {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func lockProject(ctx context.Context, q *sqlcgen.Queries, projectID int64) error {
	_, err := q.LockLiveProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrProjectNotInTenant
	}
	return err
}

// setInviteOverride maps a count in a window onto a token bucket
// (burst=count, rate=count/window). Count 0 deletes the override.
func setInviteOverride(ctx context.Context, q *sqlcgen.Queries, actor Actor, tenantID, projectID int64, kind string, count float64, windowSecs int) error {
	pid := projectID
	if count == 0 {
		return q.DeleteRateLimitOverride(ctx, sqlcgen.DeleteRateLimitOverrideParams{
			TenantID: tenantID, Kind: kind, ProjectID: &pid,
		})
	}
	p := sqlcgen.UpsertRateLimitOverrideParams{
		TenantID: tenantID, ProjectID: &pid, Kind: kind,
		Rate: count / float64(windowSecs), Burst: count,
	}
	if actor.UserID != 0 {
		p.UpdatedBy = &actor.UserID
	}
	return q.UpsertRateLimitOverride(ctx, p)
}

// SetStorageLimit sets the storage value size cap of one project, in bytes.
// 0 clears it. The cap may not exceed the tenant limit, which is the tenant
// override or platformDefault. The change and its audit row commit together.
// It returns ErrProjectNotInTenant when the project is not in the tenant.
// store, when it caches, is told to drop the tenant after the commit.
func SetStorageLimit(ctx context.Context, pool *db.Pool, store storagelimit.LimitStore, tenantID, projectID, bytes, platformDefault int64, actor Actor) error {
	if bytes < 0 {
		return ErrInvalidStorageMB
	}
	err := pool.Q(db.WithTenant(ctx, tenantID), func(tx pgx.Tx) error {
		if err := lockProject(ctx, sqlcgen.New(tx), projectID); err != nil {
			return err
		}
		if bytes > 0 {
			ceiling, err := tenantStorageCeiling(ctx, tx, tenantID, platformDefault)
			if err != nil {
				return err
			}
			if bytes > ceiling {
				return ErrStorageAboveTenant
			}
		}
		if err := storagelimit.SetTx(ctx, tx, actor.UserID, tenantID, &projectID, bytes); err != nil {
			return err
		}
		return actor.Audit(ctx, tx, tenantID, projectID, actionStorageLimitSet, strconv.FormatInt(projectID, 10),
			map[string]any{"max_value_bytes": bytes})
	})
	if err != nil {
		return err
	}
	if inv, ok := store.(storagelimit.Invalidator); ok {
		inv.Invalidate(tenantID)
	}
	return nil
}
