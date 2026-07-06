-- Player-account mirrors of dashboard_totp.sql; see the comments there for
-- the atomicity rationale on each statement.

-- name: UpsertPlayerAccountTOTPPending :one
INSERT INTO player_account_totp (player_account_id, secret_enc)
VALUES (sqlc.arg(player_account_id), sqlc.arg(secret_enc))
ON CONFLICT (player_account_id) DO UPDATE
SET secret_enc = EXCLUDED.secret_enc,
    last_used_step = 0,
    attempts = 0,
    locked_until = NULL,
    created_at = now()
WHERE player_account_totp.confirmed_at IS NULL
RETURNING player_account_id;

-- name: GetPlayerAccountTOTP :one
SELECT player_account_id, secret_enc, confirmed_at, last_used_step, attempts, locked_until
FROM player_account_totp
WHERE player_account_id = sqlc.arg(player_account_id);

-- name: ConfirmPlayerAccountTOTP :execrows
UPDATE player_account_totp
SET confirmed_at = now(),
    last_used_step = sqlc.arg(last_used_step),
    attempts = 0,
    locked_until = NULL
WHERE player_account_id = sqlc.arg(player_account_id)
  AND confirmed_at IS NULL;

-- name: DeletePlayerAccountTOTP :exec
DELETE FROM player_account_totp
WHERE player_account_id = sqlc.arg(player_account_id);

-- name: ReservePlayerAccountTOTPAttempt :one
UPDATE player_account_totp
SET attempts = attempts + 1,
    locked_until = CASE
        WHEN attempts + 1 >= sqlc.arg(max_attempts)::int
            THEN sqlc.arg(lockout_until)::timestamptz
        ELSE locked_until
    END
WHERE player_account_id = sqlc.arg(player_account_id)
  AND attempts < sqlc.arg(max_attempts)::int
RETURNING attempts, locked_until;

-- name: ResetPlayerAccountTOTPAttempts :exec
UPDATE player_account_totp
SET attempts = 0,
    locked_until = NULL
WHERE player_account_id = sqlc.arg(player_account_id);

-- name: SetPlayerAccountTOTPLastUsedStep :execrows
UPDATE player_account_totp
SET last_used_step = sqlc.arg(last_used_step)::bigint
WHERE player_account_id = sqlc.arg(player_account_id)
  AND last_used_step < sqlc.arg(last_used_step)::bigint;

-- name: DeletePlayerAccountTOTPBackupCodes :exec
DELETE FROM player_account_totp_backup_codes
WHERE player_account_id = sqlc.arg(player_account_id);

-- name: InsertPlayerAccountTOTPBackupCode :exec
INSERT INTO player_account_totp_backup_codes (player_account_id, code_hash)
VALUES (sqlc.arg(player_account_id), sqlc.arg(code_hash));

-- name: ConsumePlayerAccountTOTPBackupCode :one
UPDATE player_account_totp_backup_codes
SET used_at = now()
WHERE player_account_id = sqlc.arg(player_account_id)
  AND code_hash = sqlc.arg(code_hash)
  AND used_at IS NULL
RETURNING id;

-- name: CountPlayerAccountTOTPBackupCodesRemaining :one
SELECT COUNT(*)::bigint
FROM player_account_totp_backup_codes
WHERE player_account_id = sqlc.arg(player_account_id)
  AND used_at IS NULL;

-- name: CreatePlayerAccountTrustedDevice :exec
INSERT INTO player_account_trusted_devices (player_account_id, token_hash, expires_at)
VALUES (sqlc.arg(player_account_id), sqlc.arg(token_hash), sqlc.arg(expires_at));

-- name: GetPlayerAccountTrustedDevice :one
SELECT id
FROM player_account_trusted_devices
WHERE token_hash = sqlc.arg(token_hash)
  AND player_account_id = sqlc.arg(player_account_id)
  AND expires_at > now();

-- name: DeletePlayerAccountTrustedDevicesForAccount :exec
DELETE FROM player_account_trusted_devices
WHERE player_account_id = sqlc.arg(player_account_id);

-- name: DeleteExpiredPlayerAccountTrustedDevices :execrows
DELETE FROM player_account_trusted_devices
WHERE expires_at < now();

-- name: RevokeOtherPlayerAccountSessions :exec
UPDATE player_account_sessions
SET revoked_at = now()
WHERE player_account_id = sqlc.arg(player_account_id)
  AND refresh_hash <> sqlc.arg(keep_refresh_hash)
  AND revoked_at IS NULL;
