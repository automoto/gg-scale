-- Dashboard-side player management queries. Privileged: the dashboard
-- runs them as platform/tenant admin via BootstrapQ, so we filter by
-- tenant_id explicitly rather than relying on RLS.

-- name: ListPlayersForProject :many
SELECT
    u.id,
    u.external_id,
    coalesce(u.email, '')::text AS email,
    u.email_verified_at,
    u.disabled_at,
    u.created_at
FROM project_players u
JOIN projects p ON p.id = u.project_id
WHERE p.tenant_id = sqlc.arg(tenant_id)
  AND u.project_id = sqlc.arg(project_id)
  AND u.deleted_at IS NULL
  AND (sqlc.narg(email_filter)::text IS NULL OR coalesce(u.email, '')::text ILIKE '%' || sqlc.narg(email_filter)::text || '%')
ORDER BY u.created_at DESC
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountPlayersForProject :one
SELECT COUNT(*)::bigint
FROM project_players u
JOIN projects p ON p.id = u.project_id
WHERE p.tenant_id = sqlc.arg(tenant_id)
  AND u.project_id = sqlc.arg(project_id)
  AND u.deleted_at IS NULL
  AND (sqlc.narg(email_filter)::text IS NULL OR coalesce(u.email, '')::text ILIKE '%' || sqlc.narg(email_filter)::text || '%');

-- name: GetPlayerForProject :one
-- Enriched with the linked global account: remote addresses (project admins
-- may read them; publishable keys never) and tenant-ban status. player_accounts
-- and tenant_player_bans are global (no RLS), so the LEFT JOINs resolve under
-- the tenant Pool.Q used by the dashboard.
SELECT
    u.id,
    u.external_id,
    coalesce(u.email, '')::text AS email,
    u.email_verified_at,
    u.disabled_at,
    u.created_at,
    u.tenant_id,
    u.project_id,
    u.player_account_id,
    a.remote_addr_ip_lan,
    a.remote_addr_ip_public,
    a.remote_addr_dns,
    a.remote_addr_iroh,
    (b.id IS NOT NULL)::boolean AS tenant_banned
FROM project_players u
JOIN projects p ON p.id = u.project_id
LEFT JOIN player_accounts a ON a.id = u.player_account_id
LEFT JOIN tenant_player_bans b
       ON b.player_account_id = u.player_account_id AND b.tenant_id = p.tenant_id
WHERE p.tenant_id = sqlc.arg(tenant_id)
  AND u.project_id = sqlc.arg(project_id)
  AND u.id = sqlc.arg(id)
  AND u.deleted_at IS NULL;

-- name: SetPlayerDisabledInProject :exec
-- Project-level disable (NOT tenant-wide — a tenant-wide ban lives in
-- tenant_player_bans). Bumps session_epoch so live JWTs are rejected at
-- server-verify immediately.
UPDATE project_players
SET disabled_at   = sqlc.arg(disabled_at),
    session_epoch = session_epoch + 1
WHERE id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND tenant_id = sqlc.arg(tenant_id)
  AND deleted_at IS NULL;
