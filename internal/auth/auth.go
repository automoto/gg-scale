// Package auth signs and verifies the short-lived JWT issued to players
// after login (or anonymous signup). Tokens carry tenant_id, project_id,
// and player_id; the player middleware verifies them and asserts the
// tenant_id matches the api_key's tenant context to prevent cross-tenant
// replay.
//
// Signing is HMAC-SHA256 with a single global key; per-tenant signing keys
// with rotation land in v1.1.
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

// leeway absorbs clock skew between the signing and verifying hosts in a
// multi-region deployment; negligible next to the short token TTL.
const leeway = 30 * time.Second

// Claims are the application-meaningful fields the JWT carries. The
// jti claim is filled with a random nonce in Sign so two tokens for the
// same claims are distinguishable.
type Claims struct {
	PlayerID  int64
	TenantID  int64
	ProjectID int64 // 0 when the session has no project pin
	// SessionEpoch snapshots project_players.session_epoch at issuance. Server-side
	// verify rejects the token if the DB epoch has moved past it (disable /
	// tenant ban), so revocation is immediate rather than TTL-bounded.
	SessionEpoch int64
	// ExpiresAt must be in the future at Sign time; Sign rejects zero or
	// past values.
	ExpiresAt time.Time
}

// wireClaims is the on-the-wire JWT shape: the registered claims plus this
// package's private claims (RFC 7519 §4.3).
type wireClaims struct {
	jwt.RegisteredClaims
	PlayerID     int64 `json:"puid"`
	TenantID     int64 `json:"tid"`
	ProjectID    int64 `json:"pid,omitempty"`
	SessionEpoch int64 `json:"sepoch,omitempty"`
}

// Signer issues and verifies HMAC-SHA256 JWTs. A Signer is immutable after
// construction and safe for concurrent use.
type Signer struct {
	key []byte
}

// NewSigner constructs a Signer from a raw HMAC key. The key must be at
// least 32 bytes (the SHA-256 output size, RFC 2104's recommended minimum) —
// shorter keys are rejected at construction so misconfiguration fails fast.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < minKeyLen {
		return nil, fmt.Errorf("auth: signing key must be at least %d bytes (got %d)", minKeyLen, len(key))
	}
	return &Signer{key: key}, nil
}

// NewSignerFromHex parses a hex-encoded key. The key must be non-empty and
// decode to at least 32 bytes; zero-config deployments get a persistent key
// through Load instead.
func NewSignerFromHex(s string) (*Signer, error) {
	if s == "" {
		return nil, errors.New("auth: signing key is empty")
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("auth: hex decode signing key: %w", err)
	}
	return NewSigner(key)
}

// NewSignerRandom generates a fresh random key. Tokens issued under it
// don't survive process restarts; for tests and dev harnesses only.
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
	now := time.Now()
	if !c.ExpiresAt.After(now) {
		return "", fmt.Errorf("auth: claims ExpiresAt must be in the future (got %v)", c.ExpiresAt)
	}

	rc := wireClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(c.ExpiresAt),
			ID:        rand.Text(),
		},
		PlayerID:     c.PlayerID,
		TenantID:     c.TenantID,
		ProjectID:    c.ProjectID,
		SessionEpoch: c.SessionEpoch,
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
	rc := &wireClaims{}
	_, err := jwt.ParseWithClaims(token, rc, func(t *jwt.Token) (any, error) {
		return s.key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(leeway),
	)
	if errors.Is(err, jwt.ErrTokenExpired) {
		return Claims{}, ErrTokenExpired
	}
	if err != nil {
		return Claims{}, fmt.Errorf("auth: verify: %w", err)
	}

	out := Claims{
		PlayerID:     rc.PlayerID,
		TenantID:     rc.TenantID,
		ProjectID:    rc.ProjectID,
		SessionEpoch: rc.SessionEpoch,
	}
	if rc.ExpiresAt != nil {
		out.ExpiresAt = rc.ExpiresAt.Time
	}
	return out, nil
}
