-- name: IncrementRelaySessions :one
-- Counts one managed-relay credential issuance for the tenant month. The
-- allowance check and the increment are one atomic statement: past the
-- allowance the conflict-update WHERE fails and no row returns (refuse new
-- issuance). A negative allowance means unmetered. Runs in tenant RLS context.
INSERT INTO relay_session_usage (tenant_id, month, sessions)
VALUES (current_setting('app.tenant_id', true)::bigint, sqlc.arg(month), 1)
ON CONFLICT (tenant_id, month) DO UPDATE
SET sessions = relay_session_usage.sessions + 1,
    updated_at = now()
WHERE sqlc.arg(allowance)::bigint < 0
   OR relay_session_usage.sessions < sqlc.arg(allowance)::bigint
RETURNING sessions;

-- name: MarkRelayUsageWarned80 :execrows
-- Claims the right to send the single 80% warning email for this month
-- (0 rows = another request already sent it).
UPDATE relay_session_usage
SET warned_80_at = now(), updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND month = sqlc.arg(month)
  AND warned_80_at IS NULL;

-- name: MarkRelayUsageWarned100 :execrows
-- Claims the right to send the single 100% warning email for this month.
UPDATE relay_session_usage
SET warned_100_at = now(), updated_at = now()
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND month = sqlc.arg(month)
  AND warned_100_at IS NULL;

-- name: GetRelaySessionUsage :one
SELECT sessions
FROM relay_session_usage
WHERE tenant_id = current_setting('app.tenant_id', true)::bigint
  AND month = sqlc.arg(month);
