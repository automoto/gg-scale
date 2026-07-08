package twofactor

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Verify outcomes. Callers map these to their surface-specific HTTP responses;
// they are the single source of truth for the challenge result so the control panel
// and player surfaces cannot drift on lockout/replay semantics.
var (
	// ErrBadCode is a wrong, malformed, or replayed code.
	ErrBadCode = errors.New("twofactor: incorrect code")
	// ErrLocked means the attempt cap is reached and the credential is locked.
	ErrLocked = errors.New("twofactor: too many attempts")
	// ErrUnavailable means the stored secret could not be opened by any
	// configured key (operator changed or removed key material).
	ErrUnavailable = errors.New("twofactor: credential unavailable")
)

// Credential is the stored TOTP state the verify flow reads. It is ID-type
// agnostic so one flow serves both the int64 control panel users and the UUID
// player accounts.
type Credential struct {
	SecretEnc   []byte
	Confirmed   bool
	Locked      bool
	LockedUntil time.Time
}

// Store is the per-credential persistence the verify flow drives. Each method
// acts on the single credential the adapter is bound to, keeping the shared
// logic free of ID types and sqlc query names.
type Store interface {
	// Credential loads the row; found is false when the user has no TOTP.
	Credential(ctx context.Context) (cred Credential, found bool, err error)
	// ReserveAttempt consumes one attempt, returning ErrLocked at the cap.
	ReserveAttempt(ctx context.Context) error
	// ResetAttempts clears the attempt counter and any lockout.
	ResetAttempts(ctx context.Context) error
	// SetLastUsedStep advances the replay guard to step; rows==0 means the
	// step was already consumed (a replay), which is the atomic, race-free
	// arbiter of code reuse.
	SetLastUsedStep(ctx context.Context, step int64) (rows int64, err error)
	// ConsumeBackupCode spends a single backup code; consumed is false when
	// the code is unknown or already spent.
	ConsumeBackupCode(ctx context.Context, hash []byte) (consumed bool, err error)
}

// Verify runs the shared challenge flow: confirmed-credential and lockout
// checks, attempt reservation, then either TOTP validation with atomic replay
// arbitration or backup-code consumption. It returns the method used
// ("totp" | "backup_code"). A nil cipher means the feature is off. A valid code
// always releases its reserved attempt, so only wrong codes count toward the
// lockout.
func Verify(ctx context.Context, cipher *Cipher, s Store, code string, now time.Time, allowBackup bool) (string, error) {
	if cipher == nil {
		return "", ErrBadCode
	}
	cred, found, err := s.Credential(ctx)
	if err != nil {
		return "", err
	}
	if !found || !cred.Confirmed {
		return "", ErrBadCode
	}
	if cred.Locked {
		if now.Before(cred.LockedUntil) {
			return "", ErrLocked
		}
		// Lockout has lapsed: clear it so the attempt budget starts fresh.
		if err := s.ResetAttempts(ctx); err != nil {
			return "", err
		}
	}
	if err := s.ReserveAttempt(ctx); err != nil {
		return "", err
	}
	if IsTOTPCode(code) {
		return verifyTOTP(ctx, cipher, s, cred, code, now)
	}
	if !allowBackup {
		return "", ErrBadCode
	}
	return consumeBackup(ctx, s, code)
}

func verifyTOTP(ctx context.Context, cipher *Cipher, s Store, cred Credential, code string, now time.Time) (string, error) {
	secret, err := cipher.Decrypt(cred.SecretEnc)
	if err != nil {
		slog.ErrorContext(ctx, "two-factor secret decrypt failed; key material changed?", "err", err)
		return "", ErrUnavailable
	}
	step, ok := ValidateCode(string(secret), code, now)
	if !ok {
		return "", ErrBadCode
	}
	rows, err := s.SetLastUsedStep(ctx, step)
	if err != nil {
		return "", err
	}
	if rows == 0 {
		// The code was valid but its timestep is already consumed (a replay of
		// an accepted code). Since it validated, release the reserved attempt
		// so retries don't drive the credential into lockout, then reject reuse.
		if err := s.ResetAttempts(ctx); err != nil {
			return "", err
		}
		return "", ErrBadCode
	}
	if err := s.ResetAttempts(ctx); err != nil {
		return "", err
	}
	return "totp", nil
}

func consumeBackup(ctx context.Context, s Store, code string) (string, error) {
	consumed, err := s.ConsumeBackupCode(ctx, HashBackupCode(code))
	if err != nil {
		return "", err
	}
	if !consumed {
		return "", ErrBadCode
	}
	if err := s.ResetAttempts(ctx); err != nil {
		return "", err
	}
	return "backup_code", nil
}
