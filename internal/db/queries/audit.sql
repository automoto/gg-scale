-- name: WriteAudit :exec
INSERT INTO audit_log (tenant_id, actor_user_id, action, target, payload, actor_service)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    sqlc.narg(actor_user_id), sqlc.arg(action), sqlc.narg(target), sqlc.arg(payload), sqlc.narg(actor_service)
);

-- name: WritePlatformAudit :exec
INSERT INTO platform_audit_log (actor_user_id, action, target, payload, actor_service)
VALUES (sqlc.narg(actor_user_id), sqlc.arg(action), sqlc.narg(target), sqlc.arg(payload), sqlc.narg(actor_service));
