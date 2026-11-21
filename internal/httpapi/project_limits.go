package httpapi

import (
	"context"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/managementtoken"
	"github.com/automoto/gg-scale/internal/projectadmin"
	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/ratelimit"
)

const bytesPerMiB = 1048576

type projectInviteLimits struct {
	InviterPerHour        float64 `json:"inviter_per_hour" doc:"Invites that one inviter may send each hour in this Game Project. 0 means the default."`
	DomainPerDay          float64 `json:"domain_per_day" doc:"Invites that one email domain may get each day in this Game Project. 0 means the default."`
	DefaultInviterPerHour float64 `json:"default_inviter_per_hour" doc:"The default, and the highest value that can be set."`
	DefaultDomainPerDay   float64 `json:"default_domain_per_day" doc:"The default, and the highest value that can be set."`
}

type projectStorageLimits struct {
	MaxValueMB          float64 `json:"max_value_mb" doc:"The limit of this Game Project for one storage value, in MiB. 0 means no limit of its own."`
	EffectiveMaxValueMB float64 `json:"effective_max_value_mb" doc:"The limit that applies now."`
	TenantMaxValueMB    float64 `json:"tenant_max_value_mb" doc:"The limit of the Account Tenant. The limit of this Game Project cannot be higher."`
}

type projectLimitUsage struct {
	OpenSessions projectQuotaCount `json:"open_sessions"`
	Leaderboards projectQuotaCount `json:"leaderboards"`
}

// projectLimits is the result of the limit reads and writes.
type projectLimits struct {
	Invites projectInviteLimits  `json:"invites"`
	Storage projectStorageLimits `json:"storage"`
	Usage   projectLimitUsage    `json:"usage"`
}

func mib(bytes int64) float64 { return float64(bytes) / bytesPerMiB }

func readProjectLimits(ctx context.Context, d Deps, p managementtoken.Principal) (projectLimits, error) {
	limits, err := projectadmin.GetLimits(ctx, d.Pool, p.TenantID, p.ProjectID, storageLimit(d))
	if err != nil {
		return projectLimits{}, err
	}
	var (
		qc           sqlcgen.GetTenantQuotaContextRow
		openSessions int64
		boards       int64
	)
	err = projectQ(ctx, d, p, func(ctx context.Context, q *sqlcgen.Queries) error {
		var err error
		if qc, err = q.GetTenantQuotaContext(ctx); err != nil {
			return err
		}
		if openSessions, err = q.CountOpenGameSessionsForProject(ctx, p.ProjectID); err != nil {
			return err
		}
		boards, err = q.CountLiveLeaderboardsForProject(ctx, p.ProjectID)
		return err
	})
	if err != nil {
		return projectLimits{}, err
	}
	tier, err := quota.ResolveSnapshot(int(qc.Tier), qc.Overrides)
	if err != nil {
		return projectLimits{}, err
	}
	defaults := ratelimit.DefaultInviteLimits
	return projectLimits{
		Invites: projectInviteLimits{
			InviterPerHour: limits.InviterPerHour, DomainPerDay: limits.DomainPerDay,
			DefaultInviterPerHour: defaults.InviterPerHour, DefaultDomainPerDay: defaults.DomainPerDay,
		},
		Storage: projectStorageLimits{
			MaxValueMB:          mib(limits.StorageBytes),
			EffectiveMaxValueMB: mib(limits.EffectiveStorageBytes()),
			TenantMaxValueMB:    mib(limits.TenantStorageBytes),
		},
		Usage: projectLimitUsage{
			OpenSessions: projectQuotaCount{Current: openSessions, Limit: tier.OpenSessionsPerProject},
			Leaderboards: projectQuotaCount{Current: boards, Limit: tier.LeaderboardsPerProject},
		},
	}, nil
}

func getProjectLimits(ctx context.Context, d Deps, p managementtoken.Principal, _ *struct{}) (projectLimits, error) {
	return readProjectLimits(ctx, d, p)
}

type projectSetInviteLimitsInput struct {
	Body struct {
		InviterPerHour float64 `json:"inviter_per_hour" doc:"Invites that one inviter may send each hour. 0 clears the limit, so the default applies." example:"20"`
		DomainPerDay   float64 `json:"domain_per_day" doc:"Invites that one email domain may get each day. 0 clears the limit, so the default applies." example:"100"`
	}
}

func setProjectInviteLimits(ctx context.Context, d Deps, p managementtoken.Principal, in *projectSetInviteLimitsInput) (projectLimits, error) {
	err := projectadmin.SetInviteLimits(ctx, d.Pool, d.RateLimitOverrides, p.TenantID, p.ProjectID,
		in.Body.InviterPerHour, in.Body.DomainPerDay, projectActor(p))
	if err != nil {
		return projectLimits{}, projectWriteError(err)
	}
	return readProjectLimits(ctx, d, p)
}

type projectSetStorageLimitInput struct {
	Body struct {
		MaxValueMB float64 `json:"max_value_mb" doc:"The limit for one storage value, in MiB. 0 clears the limit of this Game Project." example:"0.5"`
	}
}

func setProjectStorageLimit(ctx context.Context, d Deps, p managementtoken.Principal, in *projectSetStorageLimitInput) (projectLimits, error) {
	bytes, err := projectadmin.StorageBytesFromMB(in.Body.MaxValueMB)
	if err != nil {
		return projectLimits{}, projectWriteError(err)
	}
	err = projectadmin.SetStorageLimit(ctx, d.Pool, d.StorageLimits, p.TenantID, p.ProjectID, bytes, storageLimit(d), projectActor(p))
	if err != nil {
		return projectLimits{}, projectWriteError(err)
	}
	return readProjectLimits(ctx, d, p)
}
