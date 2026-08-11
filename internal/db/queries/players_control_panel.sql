-- Control-panel-side player management queries. Privileged: the control panel
-- runs them as platform/tenant admin via BootstrapQ, so we filter by
-- tenant_id explicitly rather than relying on RLS.

-- name: ListPlayersForProject :many
SELECT
    u.id,
    u.external_id,
    coalesce(u.email, '')::text AS email,
    u.email_verified_at,
    u.disabled_at,
    u.delete_requested_at,
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
-- the tenant Pool.Q used by the control panel.
SELECT
    u.id,
    u.external_id,
    coalesce(u.email, '')::text AS email,
    u.email_verified_at,
    u.disabled_at,
    u.delete_requested_at,
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

-- name: SetPlayerDisabledInProject :execrows
-- Project-level disable (NOT tenant-wide — a tenant-wide ban lives in
-- tenant_player_bans). Bumps session_epoch so live JWTs are rejected at
-- server-verify immediately. A pending delete request owns the disabled
-- state: 0 rows on an existing player means "cancel the deletion first".
UPDATE project_players
SET disabled_at   = sqlc.arg(disabled_at),
    session_epoch = session_epoch + 1
WHERE id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND tenant_id = sqlc.arg(tenant_id)
  AND deleted_at IS NULL
  AND delete_requested_at IS NULL;

-- name: RequestPlayerDeleteInProject :one
-- Admin-side delete request: disables the player (keeping an earlier
-- suspension timestamp intact) and stamps delete_requested_at with the same
-- now() so cancel can tell the two apart. 0 rows = gone or already pending.
UPDATE project_players
SET delete_requested_at = now(),
    disabled_at   = COALESCE(disabled_at, now()),
    session_epoch = session_epoch + 1
WHERE id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND tenant_id = sqlc.arg(tenant_id)
  AND deleted_at IS NULL
  AND delete_requested_at IS NULL
RETURNING delete_requested_at;

-- name: CancelPlayerDeleteInProject :execrows
-- Clears the pending request; lifts the disable only when the request created
-- it (disabled_at = delete_requested_at), so a pre-existing admin suspension
-- survives the cancel. SET expressions read pre-update values, so the CASE
-- sees delete_requested_at before it is cleared.
UPDATE project_players
SET disabled_at = CASE WHEN disabled_at = delete_requested_at THEN NULL ELSE disabled_at END,
    delete_requested_at = NULL
WHERE id = sqlc.arg(id)
  AND project_id = sqlc.arg(project_id)
  AND tenant_id = sqlc.arg(tenant_id)
  AND deleted_at IS NULL
  AND delete_requested_at IS NOT NULL;
