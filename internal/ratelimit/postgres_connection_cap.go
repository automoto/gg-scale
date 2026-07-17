package ratelimit

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ggscale/ggscale/internal/cache"
	"github.com/ggscale/ggscale/internal/db"
)

type postgresGrantStore struct {
	pool *db.Pool
}

func newPostgresGrantStore(pool *db.Pool) *postgresGrantStore {
	return &postgresGrantStore{pool: pool}
}

// NewPostgresConnectionCap creates one process-owned regional capacity
// manager. The holder ID includes a boot UUID so overlapping Dokku generations
// on the same host never share a lease.
func NewPostgresConnectionCap(pool *db.Pool, region string, reg prometheus.Registerer) *LeasedConnectionCap {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown"
	}
	return newLeasedConnectionCap(newPostgresGrantStore(pool), reg, leasedCapOptions{
		Region:        region,
		HolderID:      hostname + "/" + uuid.NewString(),
		Lease:         defaultGrantLease,
		RenewInterval: defaultRenewInterval,
	})
}

func (s *postgresGrantStore) Sync(ctx context.Context, req grantRequest) (grantResult, error) {
	if req.TenantID <= 0 {
		return grantResult{}, fmt.Errorf("tenant ID must be positive")
	}
	if req.Region == "" || req.HolderID == "" {
		return grantResult{}, fmt.Errorf("region and holder ID are required")
	}
	if req.Caps.Sustained <= 0 || req.Caps.Ceiling < req.Caps.Sustained {
		return grantResult{}, fmt.Errorf("invalid connection caps: sustained=%d ceiling=%d", req.Caps.Sustained, req.Caps.Ceiling)
	}
	if req.Used < 0 || req.Requested < 0 || req.Lease <= 0 {
		return grantResult{}, fmt.Errorf("invalid grant request")
	}

	var result grantResult
	tenantCtx := db.WithTenant(ctx, req.TenantID)
	err := s.pool.Q(tenantCtx, func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(tenantCtx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
			return fmt.Errorf("connection grant clock: %w", err)
		}
		if _, err := tx.Exec(tenantCtx, `
INSERT INTO realtime_connection_cap_states (
    tenant_id, region, sustained, ceiling, burst_remaining_ns, last_assessed_at
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tenant_id, region) DO NOTHING`,
			req.TenantID, req.Region, req.Caps.Sustained, req.Caps.Ceiling,
			int64(ConnectionBurstBudget), now,
		); err != nil {
			return fmt.Errorf("connection grant initialize state: %w", err)
		}

		var persistedSustained, persistedCeiling, burstNanos int64
		var lastAssessed time.Time
		if err := tx.QueryRow(tenantCtx, `
SELECT sustained, ceiling, burst_remaining_ns, last_assessed_at
FROM realtime_connection_cap_states
WHERE tenant_id = $1 AND region = $2
FOR UPDATE`, req.TenantID, req.Region).Scan(
			&persistedSustained, &persistedCeiling, &burstNanos, &lastAssessed,
		); err != nil {
			return fmt.Errorf("connection grant lock state: %w", err)
		}

		if _, err := tx.Exec(tenantCtx, `
DELETE FROM realtime_connection_grants
WHERE tenant_id = $1 AND region = $2 AND expires_at <= $3`,
			req.TenantID, req.Region, now,
		); err != nil {
			return fmt.Errorf("connection grant prune: %w", err)
		}

		var totalAllocated, totalUsed int64
		if err := tx.QueryRow(tenantCtx, `
SELECT COALESCE(SUM(allocated), 0), COALESCE(SUM(used), 0)
FROM realtime_connection_grants
WHERE tenant_id = $1 AND region = $2`,
			req.TenantID, req.Region,
		).Scan(&totalAllocated, &totalUsed); err != nil {
			return fmt.Errorf("connection grant totals: %w", err)
		}

		var holderAllocated, holderUsed int64
		err := tx.QueryRow(tenantCtx, `
SELECT allocated, used
FROM realtime_connection_grants
WHERE tenant_id = $1 AND region = $2 AND holder_id = $3`,
			req.TenantID, req.Region, req.HolderID,
		).Scan(&holderAllocated, &holderUsed)
		if err != nil && err != pgx.ErrNoRows {
			return fmt.Errorf("connection grant holder: %w", err)
		}

		burst := cache.BurstSlotState{
			Count:          totalUsed,
			BurstRemaining: time.Duration(burstNanos),
			LastAssessed:   lastAssessed,
			Sustained:      persistedSustained,
			BurstBudget:    ConnectionBurstBudget,
		}
		cache.AssessBurst(&burst, now)
		burst.Sustained = req.Caps.Sustained
		burst.BurstBudget = ConnectionBurstBudget
		burst.BurstRemaining = min(burst.BurstRemaining, ConnectionBurstBudget)

		otherAllocated := totalAllocated - holderAllocated
		limit := req.Caps.Ceiling
		reason := CapRejectCeiling
		if req.Caps.Ceiling > req.Caps.Sustained && burst.BurstRemaining <= 0 {
			limit = req.Caps.Sustained
			reason = CapRejectBudget
		}
		maxForHolder := max(int64(0), limit-otherAllocated)
		desired := req.Used + req.Requested
		allocated := min(desired, maxForHolder)
		// Established sockets are never dropped to repair an expired or stale
		// grant. Preserve their count and stop issuing new permits until the
		// regional total returns below the configured wall.
		allocated = max(allocated, req.Used)

		expiresAt := now.Add(req.Lease)
		if allocated > 0 || req.Used > 0 {
			if _, err := tx.Exec(tenantCtx, `
INSERT INTO realtime_connection_grants (
    tenant_id, region, holder_id, allocated, used, expires_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, region, holder_id) DO UPDATE SET
    allocated = EXCLUDED.allocated,
    used = EXCLUDED.used,
    expires_at = EXCLUDED.expires_at,
    updated_at = EXCLUDED.updated_at`,
				req.TenantID, req.Region, req.HolderID, allocated, req.Used, expiresAt, now,
			); err != nil {
				return fmt.Errorf("connection grant upsert: %w", err)
			}
		}

		if _, err := tx.Exec(tenantCtx, `
UPDATE realtime_connection_cap_states
SET sustained = $3,
    ceiling = $4,
    burst_remaining_ns = $5,
    last_assessed_at = $6,
    updated_at = $6
WHERE tenant_id = $1 AND region = $2`,
			req.TenantID, req.Region, req.Caps.Sustained, req.Caps.Ceiling,
			int64(burst.BurstRemaining), now,
		); err != nil {
			return fmt.Errorf("connection grant update state: %w", err)
		}

		current := totalUsed - holderUsed + req.Used
		result = grantResult{
			Allocated: allocated,
			Current:   max(int64(0), current),
		}
		if allocated <= req.Used {
			result.Reason = reason
		}
		return nil
	})
	if err != nil {
		return grantResult{}, err
	}
	return result, nil
}

func (s *postgresGrantStore) Release(ctx context.Context, req grantRelease) error {
	if req.TenantID <= 0 || req.Region == "" || req.HolderID == "" {
		return fmt.Errorf("invalid connection grant release")
	}
	tenantCtx := db.WithTenant(ctx, req.TenantID)
	return s.pool.Q(tenantCtx, func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(tenantCtx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
			return fmt.Errorf("connection grant release clock: %w", err)
		}

		var sustained, burstNanos int64
		var lastAssessed time.Time
		err := tx.QueryRow(tenantCtx, `
SELECT sustained, burst_remaining_ns, last_assessed_at
FROM realtime_connection_cap_states
WHERE tenant_id = $1 AND region = $2
FOR UPDATE`, req.TenantID, req.Region).Scan(&sustained, &burstNanos, &lastAssessed)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("connection grant release lock state: %w", err)
		}

		var totalUsed int64
		if err := tx.QueryRow(tenantCtx, `
SELECT COALESCE(SUM(used), 0)
FROM realtime_connection_grants
WHERE tenant_id = $1 AND region = $2 AND expires_at > $3`,
			req.TenantID, req.Region, now,
		).Scan(&totalUsed); err != nil {
			return fmt.Errorf("connection grant release total: %w", err)
		}

		burst := cache.BurstSlotState{
			Count:          totalUsed,
			BurstRemaining: time.Duration(burstNanos),
			LastAssessed:   lastAssessed,
			Sustained:      sustained,
			BurstBudget:    ConnectionBurstBudget,
		}
		cache.AssessBurst(&burst, now)
		if _, err := tx.Exec(tenantCtx, `
DELETE FROM realtime_connection_grants
WHERE tenant_id = $1 AND region = $2 AND holder_id = $3`,
			req.TenantID, req.Region, req.HolderID,
		); err != nil {
			return fmt.Errorf("connection grant release delete: %w", err)
		}
		if _, err := tx.Exec(tenantCtx, `
UPDATE realtime_connection_cap_states
SET burst_remaining_ns = $3, last_assessed_at = $4, updated_at = $4
WHERE tenant_id = $1 AND region = $2`,
			req.TenantID, req.Region, int64(burst.BurstRemaining), now,
		); err != nil {
			return fmt.Errorf("connection grant release state: %w", err)
		}
		return nil
	})
}

func (s *postgresGrantStore) Renew(ctx context.Context, req grantRenewRequest) (map[int64]struct{}, error) {
	if req.Region == "" || req.HolderID == "" || req.Lease <= 0 {
		return nil, fmt.Errorf("invalid connection grant renewal")
	}
	if len(req.Grants) == 0 {
		return map[int64]struct{}{}, nil
	}
	tenantIDs := make([]int64, 0, len(req.Grants))
	used := make([]int64, 0, len(req.Grants))
	for _, grant := range req.Grants {
		if grant.TenantID <= 0 || grant.Used < 0 {
			return nil, fmt.Errorf("invalid connection grant renewal entry")
		}
		tenantIDs = append(tenantIDs, grant.TenantID)
		used = append(used, grant.Used)
	}

	renewed := make(map[int64]struct{}, len(req.Grants))
	err := s.pool.BootstrapQ(ctx, func(tx pgx.Tx) error {
		var now time.Time
		if err := tx.QueryRow(ctx, "SELECT transaction_timestamp()").Scan(&now); err != nil {
			return fmt.Errorf("connection grant renewal clock: %w", err)
		}
		expiresAt := now.Add(req.Lease)
		rows, err := tx.Query(ctx, `
WITH renewals AS (
    SELECT * FROM unnest($4::bigint[], $5::bigint[]) AS r(tenant_id, used)
)
UPDATE realtime_connection_grants AS g
SET used = r.used, expires_at = $6, updated_at = $3
FROM renewals AS r
WHERE g.region = $1
  AND g.holder_id = $2
  AND g.tenant_id = r.tenant_id
  AND g.expires_at > $3
RETURNING g.tenant_id`,
			req.Region, req.HolderID, now, tenantIDs, used, expiresAt,
		)
		if err != nil {
			return fmt.Errorf("connection grant renew: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var tenantID int64
			if err := rows.Scan(&tenantID); err != nil {
				return fmt.Errorf("connection grant renew result: %w", err)
			}
			renewed[tenantID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("connection grant renew rows: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return renewed, nil
}
