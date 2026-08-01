-- name: GetProjectSteamAuthConfig :one
-- Runtime read for the Steam sign-in endpoint, tenant-scoped via RLS GUC.
SELECT steam_app_id, steam_web_api_key
FROM projects
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: GetProjectSteamAuthConfigForControlPanel :one
-- Control-panel read: reports whether a key is stored, never its value.
SELECT steam_app_id,
       (steam_web_api_key IS NOT NULL AND length(steam_web_api_key) > 0) AS steam_key_configured
FROM projects
WHERE id = sqlc.arg(project_id)
  AND tenant_id = sqlc.arg(tenant_id)
  AND deleted_at IS NULL;

-- name: UpdateProjectSteamAuthConfig :execrows
-- A NULL key keeps the stored one, so the settings form can update the app id
-- without re-entering the secret (replace-on-write).
UPDATE projects
SET steam_app_id = sqlc.arg(steam_app_id),
    steam_web_api_key = COALESCE(sqlc.narg(steam_web_api_key), steam_web_api_key)
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;

-- name: ClearProjectSteamAuthConfig :execrows
UPDATE projects
SET steam_app_id = '', steam_web_api_key = NULL
WHERE id = sqlc.arg(project_id)
  AND tenant_id = current_setting('app.tenant_id', true)::bigint
  AND deleted_at IS NULL;
