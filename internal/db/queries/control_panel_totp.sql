-- name: UpsertControlPanelTOTPPending :one
-- Starts (or restarts) enrollment. The WHERE guard makes this a no-op for a
-- confirmed credential — zero rows means "already enabled", so a stray setup
-- POST can never silently replace a live secret.
INSERT INTO control_panel_user_totp (control_panel_user_id, secret_enc)
VALUES (sqlc.arg(control_panel_user_id), sqlc.arg(secret_enc))
ON CONFLICT (control_panel_user_id) DO UPDATE
SET secret_enc = EXCLUDED.secret_enc,
    last_used_step = 0,
    attempts = 0,
    locked_until = NULL,
    created_at = now()
WHERE control_panel_user_totp.confirmed_at IS NULL
RETURNING control_panel_user_id;

-- name: GetControlPanelTOTP :one
SELECT control_panel_user_id, secret_enc, confirmed_at, last_used_step, attempts, locked_until
FROM control_panel_user_totp
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id);

-- name: ConfirmControlPanelTOTP :execrows
-- last_used_step records the enrollment code's timestep so the same code
-- cannot be replayed at the first login challenge.
UPDATE control_panel_user_totp
SET confirmed_at = now(),
    last_used_step = sqlc.arg(last_used_step),
    attempts = 0,
    locked_until = NULL
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND confirmed_at IS NULL;

-- name: DeleteControlPanelTOTP :exec
-- Backup codes cascade with the credential row.
DELETE FROM control_panel_user_totp
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id);

-- name: ReserveControlPanelTOTPAttempt :one
-- Atomic check-and-bump, same shape as ReserveControlPanelVerifyAttempt: the
-- cap lives in the WHERE so parallel wrong codes cannot overshoot it.
-- Returns 0 rows when already at cap (caller treats as locked).
UPDATE control_panel_user_totp
SET attempts = attempts + 1,
    locked_until = CASE
        WHEN attempts + 1 >= sqlc.arg(max_attempts)::int
            THEN sqlc.arg(lockout_until)::timestamptz
        ELSE locked_until
    END
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND attempts < sqlc.arg(max_attempts)::int
RETURNING attempts, locked_until;

-- name: ResetControlPanelTOTPAttempts :exec
UPDATE control_panel_user_totp
SET attempts = 0,
    locked_until = NULL
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id);

-- name: SetControlPanelTOTPLastUsedStep :execrows
-- The monotonic guard in the WHERE makes replay detection atomic: 0 rows
-- means another request already consumed this timestep.
UPDATE control_panel_user_totp
SET last_used_step = sqlc.arg(last_used_step)::bigint
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND last_used_step < sqlc.arg(last_used_step)::bigint;

-- name: DeleteControlPanelTOTPBackupCodes :exec
DELETE FROM control_panel_user_totp_backup_codes
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id);

-- name: InsertControlPanelTOTPBackupCodes :copyfrom
-- Bulk-inserts a fresh backup-code set in one round-trip (pgx COPY).
INSERT INTO control_panel_user_totp_backup_codes (control_panel_user_id, code_hash)
VALUES ($1, $2);

-- name: ConsumeControlPanelTOTPBackupCode :one
-- Single-use enforced by the used_at IS NULL guard in the same statement
-- that marks it used. 0 rows = unknown or already-spent code.
UPDATE control_panel_user_totp_backup_codes
SET used_at = now()
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND code_hash = sqlc.arg(code_hash)
  AND used_at IS NULL
RETURNING id;

-- name: CountControlPanelTOTPBackupCodesRemaining :one
SELECT COUNT(*)::bigint
FROM control_panel_user_totp_backup_codes
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND used_at IS NULL;

-- name: CreateControlPanelTrustedDevice :exec
INSERT INTO control_panel_trusted_devices (control_panel_user_id, token_hash, expires_at, ip, user_agent)
VALUES (
    sqlc.arg(control_panel_user_id),
    sqlc.arg(token_hash),
    sqlc.arg(expires_at),
    sqlc.narg(ip),
    sqlc.narg(user_agent)
);

-- name: GetControlPanelTrustedDevice :one
-- Keyed by user AND token so a token minted for one account can never
-- satisfy another's challenge.
SELECT id
FROM control_panel_trusted_devices
WHERE token_hash = sqlc.arg(token_hash)
  AND control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND expires_at > now();

-- name: DeleteControlPanelTrustedDevicesForUser :exec
DELETE FROM control_panel_trusted_devices
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id);

-- name: DeleteExpiredControlPanelTrustedDevices :execrows
DELETE FROM control_panel_trusted_devices
WHERE expires_at < now();

-- name: RevokeOtherControlPanelSessionsForUser :exec
-- 2FA enable/disable revokes every session except the one doing the change;
-- the RevokeAll variant would log the acting user out mid-flow.
UPDATE control_panel_sessions
SET revoked_at = now()
WHERE control_panel_user_id = sqlc.arg(control_panel_user_id)
  AND id <> sqlc.arg(keep_session_id)
  AND revoked_at IS NULL;
