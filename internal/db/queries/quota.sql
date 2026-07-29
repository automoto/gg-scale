-- name: GetTenantQuotaContext :one
-- Lock-free snapshot of the current tenant's class, enforcement flag, and
-- registered-player count. Read inside an RLS-scoped tx (app.tenant_id set)
-- before a quota-gated growth operation. Deliberately NOT FOR UPDATE: the
-- authoritative player gate is ReserveTenantPlayerSlot, so this read must
-- never serialize concurrent creates on the tenant row.
SELECT tier, enforce_quotas, player_count
FROM tenants
WHERE id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: GetTenantQuotaContextLocked :one
-- FOR UPDATE variant for rare growth paths (project create, storage writes)
-- whose check-then-apply exactness relies on tenant-row serialization. Never
-- use it on the player-create path — that gate is ReserveTenantPlayerSlot,
-- and the whole point of the split is that creates don't serialize.
SELECT tier, enforce_quotas, player_count
FROM tenants
WHERE id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
FOR UPDATE;

-- name: CountProjectsForTenant :one
-- Live (non-soft-deleted) project count for the current tenant.
SELECT count(*)::bigint
FROM projects
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: ReserveTenantPlayerSlot :execrows
-- Authoritative player-quota gate: atomically claims one registered-player
-- slot by bumping player_count, refusing at the class cap (0 rows affected).
-- Run it as the LAST statement before commit so the tenant-row lock is held
-- for a single round trip instead of across the whole create transaction.
-- player_limit is quota.Limits.Players; -1 is quota.Unlimited. The
-- NOT enforce_quotas arm admits unenforced tenants while still counting
-- them, so player_count stays exact for every tenant and flipping
-- enforce_quotas on later needs no recompute. Out-of-band player writes
-- (bulk seeds, imports) must maintain the counter themselves.
UPDATE tenants
SET player_count = player_count + 1
WHERE id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL
  AND (NOT enforce_quotas
       OR sqlc.arg(player_limit)::bigint = -1
       OR player_count < sqlc.arg(player_limit)::bigint);

-- name: SetTenantEnforceQuotas :exec
-- Flip the per-tenant enforcement flag. Used by provisioning when the operator
-- has enabled quota enforcement for new tenants. player_count is maintained
-- for unenforced tenants too, so flipping this on later is safe as long as
-- players were only ever created through the app's create paths.
UPDATE tenants
SET enforce_quotas = sqlc.arg(enforce_quotas)
WHERE id = sqlc.arg(tenant_id);
