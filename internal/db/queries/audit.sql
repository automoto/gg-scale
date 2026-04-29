-- name: WriteAudit :exec
INSERT INTO audit_log (tenant_id, actor_user_id, action, target, payload)
VALUES (
    current_setting('app.tenant_id', true)::bigint,
    $1, $2, $3, $4
);
