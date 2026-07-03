-- name: GetProfile :one
SELECT id, project_id, external_id, email, xuid, email_verified_at, created_at
FROM project_players
WHERE id = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

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
