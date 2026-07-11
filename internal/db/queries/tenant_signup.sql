-- name: CreateTenantSignupRequest :one
INSERT INTO tenant_signup_requests (email, requested_tenant_name, project_description, studio_name)
VALUES (sqlc.arg(email), sqlc.arg(requested_tenant_name), sqlc.arg(project_description), sqlc.narg(studio_name))
RETURNING id, email::text AS email, requested_tenant_name, status, created_at;

-- name: GetTenantSignupRequestByID :one
SELECT id, email::text AS email, requested_tenant_name, final_tenant_name,
       project_description, studio_name, status, code_hash, code_expires_at,
       reviewed_by_user_id, reviewed_at, review_reason, tenant_id, accepted_at, created_at
FROM tenant_signup_requests
WHERE id = sqlc.arg(id);

-- name: GetTenantSignupRequestByCodeHash :one
-- Acceptance lookup: only an approved request with a live code is acceptable.
SELECT id, email::text AS email, requested_tenant_name, final_tenant_name,
       project_description, studio_name, status, code_hash, code_expires_at,
       reviewed_by_user_id, reviewed_at, review_reason, tenant_id, accepted_at, created_at
FROM tenant_signup_requests
WHERE code_hash = sqlc.arg(code_hash)
  AND status = 'approved';

-- name: ListPendingTenantSignupRequests :many
SELECT id, email::text AS email, requested_tenant_name, project_description,
       studio_name, created_at
FROM tenant_signup_requests
WHERE status = 'pending'
ORDER BY created_at ASC;

-- name: ApproveTenantSignupRequest :execrows
UPDATE tenant_signup_requests
SET status = 'approved',
    final_tenant_name = sqlc.arg(final_tenant_name),
    code_hash = sqlc.arg(code_hash),
    code_expires_at = sqlc.arg(code_expires_at),
    reviewed_by_user_id = sqlc.arg(reviewed_by_user_id),
    reviewed_at = now()
WHERE id = sqlc.arg(id)
  AND status = 'pending';

-- name: DenyTenantSignupRequest :execrows
UPDATE tenant_signup_requests
SET status = 'denied',
    review_reason = sqlc.narg(review_reason),
    reviewed_by_user_id = sqlc.arg(reviewed_by_user_id),
    reviewed_at = now()
WHERE id = sqlc.arg(id)
  AND status = 'pending';

-- name: MarkTenantSignupAccepted :exec
-- Clears the code hash so the magic link can't be replayed after acceptance.
UPDATE tenant_signup_requests
SET status = 'accepted',
    tenant_id = sqlc.arg(tenant_id),
    accepted_at = now(),
    code_hash = NULL
WHERE id = sqlc.arg(id);

-- name: TenantNameTaken :one
-- A name is taken if an active tenant has it, or another live (pending/approved)
-- signup request claims it. exclude_request_id lets the approve re-check ignore
-- the request being approved; pass 0 at submit time.
SELECT EXISTS (
    SELECT 1 FROM tenants t
    WHERE lower(t.name) = lower(sqlc.arg(name)) AND t.deleted_at IS NULL
    UNION ALL
    SELECT 1 FROM tenant_signup_requests r
    WHERE lower(COALESCE(r.final_tenant_name, r.requested_tenant_name)) = lower(sqlc.arg(name))
      AND r.status IN ('pending', 'approved')
      AND r.id <> sqlc.arg(exclude_request_id)
)::bool AS taken;

-- name: GetPublicSignupEnabled :one
SELECT public_tenant_signup_enabled
FROM platform_signup_config
WHERE id = 1;

-- name: SetPublicSignupEnabled :exec
UPDATE platform_signup_config
SET public_tenant_signup_enabled = sqlc.arg(enabled),
    updated_by = sqlc.arg(updated_by),
    updated_at = now()
WHERE id = 1;

-- name: ControlPanelCreateTenantBare :one
SELECT control_panel_create_tenant_bare(sqlc.arg(actor_user_id), sqlc.arg(tenant_name))::bigint AS tenant_id;
