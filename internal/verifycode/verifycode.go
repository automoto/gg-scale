// Package verifycode provides the shared primitives for 6-digit email
// verification codes used by both the operator dashboard and the
// player-facing player flow:
//
//   - 6-digit numeric code generation
//   - per-user salt + SHA-256 hashing (avoids precomputed rainbow tables
//     on the small 10^6 code space)
//   - expiry / attempt / resend-cooldown checks
//
// Invite-link codes use a longer URL-safe alphabet; both kinds of codes
// share the salt+hash storage convention.
package verifycode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"time"
)

const (
	// CodeTTL is the lifetime of a 6-digit verification code.
	CodeTTL = 15 * time.Minute

	// InviteTTL is the lifetime of an invitation link's URL-safe code.
	InviteTTL = 3 * 24 * time.Hour

	// ResendCooldown caps how often a new code can be sent to the same address.
	ResendCooldown = 1 * time.Minute

	// MaxAttempts is the number of wrong code submissions before the code
	// is invalidated and the user has to request a new one.
	MaxAttempts = 5

	// MaxLifetimeAttempts is the per-account cap that survives /resend.
	// Without this, an attacker could mint a fresh code each minute and
	// keep burning the MaxAttempts budget forever; this counter only
	// resets on successful verification.
	MaxLifetimeAttempts = 20

	// LockoutDuration is how long an account stays locked after hitting
	// MaxLifetimeAttempts. Operator support unlocks earlier by clearing
	// the column.
	LockoutDuration = 24 * time.Hour

	saltBytes       = 16
	inviteCodeBytes = 24
)

// codeSpace is the upper bound (exclusive) for the 6-digit code.
var codeSpace = big.NewInt(1_000_000)

// GenerateCode returns a fresh 6-digit numeric code as a string with
// leading zeros preserved. Uses crypto/rand.Int to avoid modulo bias
// (2^32 is not a multiple of 10^6).
func GenerateCode() (string, error) {
	n, err := rand.Int(rand.Reader, codeSpace)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// GenerateInviteCode returns a URL-safe random string suitable for an
// invitation magic link.
func GenerateInviteCode() (string, error) {
	buf := make([]byte, inviteCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewSalt returns a random per-user salt for code hashing.
func NewSalt() ([]byte, error) {
	buf := make([]byte, saltBytes)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// Hash returns SHA-256(salt || ":" || code).
func Hash(salt []byte, code string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(":"))
	h.Write([]byte(code))
	return h.Sum(nil)
}

// CanResend reports whether enough time has elapsed since the last send
// to allow another one.
func CanResend(lastSent, now time.Time) bool {
	if lastSent.IsZero() {
		return true
	}
	return now.Sub(lastSent) >= ResendCooldown
}

// Expired reports whether the code's expiry has passed (or was never set).
func Expired(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return true
	}
	return !expiresAt.After(now)
}

// AttemptsExhausted reports whether the attempt counter has reached
// MaxAttempts.
func AttemptsExhausted(attempts int) bool {
	return attempts >= MaxAttempts
}

// LifetimeExhausted reports whether the lifetime attempt counter has
// reached MaxLifetimeAttempts — the account is locked at this point.
func LifetimeExhausted(attempts int) bool {
	return attempts >= MaxLifetimeAttempts
}

// AccountLocked reports whether lockedUntil is in the future.
func AccountLocked(lockedUntil, now time.Time) bool {
	if lockedUntil.IsZero() {
		return false
	}
	return lockedUntil.After(now)
}
