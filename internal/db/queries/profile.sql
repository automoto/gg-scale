-- name: GetProfile :one
SELECT id, project_id, external_id, email, email_verified_at, created_at
FROM end_users
WHERE id = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: UpdateProfileEmail :exec
-- Profile updates are deliberately narrow — only fields explicitly
-- enumerated server-side may change. PATCHing email re-triggers the
-- verify flow (handler clears email_verified_at).
UPDATE end_users
SET email                          = $2,
    email_verified_at              = NULL,
    email_verification_hash        = $3,
    email_verification_expires_at  = $4
WHERE id = $1
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;
