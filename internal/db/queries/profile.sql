-- name: GetProfile :one
-- display_name lives on the linked global account; NULL for anonymous /
-- unlinked players.
SELECT p.id, p.project_id, p.external_id, p.email, p.xuid, p.email_verified_at, p.created_at,
       a.display_name
FROM project_players p
LEFT JOIN player_accounts a ON a.id = p.player_account_id
WHERE p.id = $1
  AND p.tenant_id = current_setting('app.tenant_id', true)::bigint
  AND p.deleted_at IS NULL;

-- name: UpdateProfileXuid :exec
-- Self-set secondary identifier. NULL clears it. The unique partial index
-- on (project_id, xuid) rejects collisions with a constraint violation.
UPDATE project_players
SET xuid = sqlc.narg('xuid')
WHERE id = sqlc.arg('id')
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: UpdateProfileEmail :exec
-- Profile updates are deliberately narrow — only fields explicitly
-- enumerated server-side may change. PATCHing email re-triggers the
-- verify flow (handler clears email_verified_at).
UPDATE project_players
SET email                           = $2,
    email_verified_at               = NULL,
    email_verification_code_hash    = $3,
    email_verification_salt         = $4,
    email_verification_expires_at   = $5,
    email_verification_attempts     = 0,
    email_verification_last_sent_at = now()
WHERE id = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;
