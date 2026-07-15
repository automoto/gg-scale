-- name: CreateTenantChangeRequest :one
-- Submit a tenant change request. The pending-unique index rejects a second
-- open request of the same kind/feature (surfaced as a friendly message).
INSERT INTO tenant_change_requests (tenant_id, requested_by_user_id, kind, requested_tier, feature, note)
VALUES ($1, $2, $3, sqlc.narg(requested_tier), sqlc.narg(feature), sqlc.arg(note))
RETURNING id;

-- name: ListTenantChangeRequests :many
-- The tenant's own requests (any status) for the settings page. Filters by
-- explicit tenant_id (the table has no RLS).
SELECT id, kind, requested_tier, feature, note, status, review_reason, created_at, reviewed_at
FROM tenant_change_requests
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT 50;

-- name: ListTenantEnabledFeatures :many
-- Tenant-level features the tenant already holds, to exclude from the request
-- dropdown. Reads feature_grants in tenant RLS context.
SELECT feature
FROM feature_grants
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND project_id IS NULL
  AND enabled = true;

-- name: ListPendingTenantChangeRequests :many
-- Platform-admin review queue: pending requests with tenant name + current
-- class. Read cross-tenant (bootstrap tx).
SELECT r.id, r.tenant_id, t.name AS tenant_name, t.tier AS current_tier,
       r.kind, r.requested_tier, r.feature, r.note, r.created_at
FROM tenant_change_requests r
JOIN tenants t ON t.id = r.tenant_id
WHERE r.status = 'pending'
  AND t.deleted_at IS NULL
ORDER BY r.created_at ASC;

-- name: GetTenantChangeRequestByID :one
SELECT id, tenant_id, kind, requested_tier, feature, note, status
FROM tenant_change_requests
WHERE id = $1;

-- name: ApproveTenantChangeRequest :execrows
UPDATE tenant_change_requests
SET status = 'approved',
    reviewed_by_user_id = sqlc.arg(reviewed_by),
    reviewed_at = now()
WHERE id = $1 AND status = 'pending';

-- name: DenyTenantChangeRequest :execrows
UPDATE tenant_change_requests
SET status = 'denied',
    reviewed_by_user_id = sqlc.arg(reviewed_by),
    reviewed_at = now(),
    review_reason = sqlc.narg(review_reason)
WHERE id = $1 AND status = 'pending';

-- name: SetTenantTierIfUpgrade :execrows
-- Auto-applied on tier-upgrade approval. The direction check and write are one
-- atomic operation so a stale request can never lower the current tier.
UPDATE tenants
SET tier = sqlc.arg(tier)
WHERE id = current_setting('app.tenant_id', true)::bigint
  AND tier < sqlc.arg(tier);

-- name: UpsertTenantFeatureGrant :exec
-- Auto-applied on feature-request approval. Tenant-level grant (project_id
-- NULL); runs in tenant RLS context.
INSERT INTO feature_grants (tenant_id, project_id, feature, enabled, approved_by_control_panel_user_id, reason)
VALUES (current_setting('app.tenant_id', true)::bigint, NULL, sqlc.arg(feature), true,
        sqlc.arg(approved_by), sqlc.narg(reason))
ON CONFLICT (tenant_id, COALESCE(project_id, 0), feature)
DO UPDATE SET enabled = true,
              approved_by_control_panel_user_id = EXCLUDED.approved_by_control_panel_user_id,
              reason = EXCLUDED.reason,
              updated_at = now();
