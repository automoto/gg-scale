// Package auth signs and verifies the short-lived JWT issued to end-users
// after login (or anonymous signup). Tokens carry tenant_id, project_id,
// and end_user_id; the enduser middleware verifies them and asserts the
// tenant_id matches the api_key's tenant context to prevent cross-tenant
// replay.
//
// Signing is HMAC-SHA256 with a single global key for Phase 1; per-tenant
// signing keys with rotation land in v1.1 (see docs/m1.md §4.1.3).
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrTokenExpired is returned by Verify when the token's exp has passed.
// Callers map this to 401 with a hint that the token can be refreshed.
var ErrTokenExpired = errors.New("auth: token expired")

const minKeyLen = 32

// Claims are the application-meaningful fields the JWT carries. The
// jti claim is filled with a random nonce in Sign so two tokens for the
// same claims are distinguishable.
type Claims struct {
	EndUserID int64
	TenantID  int64
	ProjectID int64 // 0 when the session has no project pin
	ExpiresAt time.Time
}

type registeredClaims struct {
	jwt.RegisteredClaims
	EndUserID int64 `json:"euid"`
	TenantID  int64 `json:"tid"`
	ProjectID int64 `json:"pid,omitempty"`
}

// Signer issues and verifies HMAC-SHA256 JWTs.
type Signer struct {
	key []byte
}

// NewSigner constructs a Signer from a raw HMAC key. The key must be at
// least 32 bytes (matches the SHA-256 block size) — shorter keys are
// rejected at construction so misconfiguration fails fast.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < minKeyLen {
		return nil, fmt.Errorf("auth: signing key must be at least %d bytes (got %d)", minKeyLen, len(key))
	}
	return &Signer{key: key}, nil
}

// NewSignerFromHex parses a hex-encoded key. Empty input falls through to
// NewSignerRandom — callers may use this to make the env var optional in
// dev. Production deploys should always supply a stable key.
func NewSignerFromHex(s string) (*Signer, error) {
	if s == "" {
		return NewSignerRandom()
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("auth: hex decode signing key: %w", err)
	}
	return NewSigner(key)
}

// NewSignerRandom generates a fresh random key. Tokens issued under it
// don't survive process restarts; useful only for tests and dev.
func NewSignerRandom() (*Signer, error) {
	key := make([]byte, minKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("auth: read random: %w", err)
	}
	return &Signer{key: key}, nil
}

// Sign emits a JWT for the given claims. A random jti is added so two
// otherwise-identical claim sets produce distinct tokens.
func (s *Signer) Sign(c Claims) (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: random nonce: %w", err)
	}

	now := time.Now()
	rc := registeredClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(c.ExpiresAt),
			ID:        hex.EncodeToString(nonce),
		},
		EndUserID: c.EndUserID,
		TenantID:  c.TenantID,
		ProjectID: c.ProjectID,
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, rc)
	out, err := tok.SignedString(s.key)
	if err != nil {
		return "", fmt.Errorf("auth: sign: %w", err)
	}
	return out, nil
}

// Verify parses and validates token. Returns ErrTokenExpired specifically
// when the only failure was expiry — callers may distinguish this from
// "broken signature" so the SDK can decide whether to refresh.
func (s *Signer) Verify(token string) (Claims, error) {
	rc := &registeredClaims{}
	tok, err := jwt.ParseWithClaims(token, rc, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %v", t.Method.Alg())
		}
		return s.key, nil
	})
	if errors.Is(err, jwt.ErrTokenExpired) {
		return Claims{}, ErrTokenExpired
	}
	if err != nil {
		return Claims{}, fmt.Errorf("auth: verify: %w", err)
	}
	if !tok.Valid {
		return Claims{}, errors.New("auth: token invalid")
	}

	exp, _ := rc.GetExpirationTime()
	out := Claims{
		EndUserID: rc.EndUserID,
		TenantID:  rc.TenantID,
		ProjectID: rc.ProjectID,
	}
	if exp != nil {
		out.ExpiresAt = exp.Time
	}
	return out, nil
}
