-- name: SuppressEmail :exec
-- Idempotent: repeated unsubscribes are a no-op.
INSERT INTO email_suppressions (email)
VALUES (sqlc.arg(email))
ON CONFLICT (email) DO NOTHING;

-- name: IsEmailSuppressed :one
SELECT EXISTS (
    SELECT 1 FROM email_suppressions WHERE email = sqlc.arg(email)
) AS suppressed;
