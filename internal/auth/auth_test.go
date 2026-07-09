package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
)

var testKey = []byte("test-key-must-be-at-least-32-bytes-long")

func newSigner(t *testing.T) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner(testKey)
	require.NoError(t, err)
	return s
}

// signRawHS256 hand-crafts a token outside Signer.Sign so tests can mint
// shapes Sign refuses to produce (already-expired, missing claims).
func signRawHS256(t *testing.T, key []byte, claims jwt.MapClaims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	require.NoError(t, err)
	return signed
}

func TestNewSigner_rejects_short_keys(t *testing.T) {
	_, err := auth.NewSigner([]byte("too-short"))

	assert.Error(t, err)
}

func TestSign_then_Verify_round_trips_claims(t *testing.T) {
	s := newSigner(t)
	want := auth.Claims{
		PlayerID:  42,
		TenantID:  7,
		ProjectID: 9,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	tok, err := s.Sign(want)
	require.NoError(t, err)

	got, err := s.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, want.PlayerID, got.PlayerID)
	assert.Equal(t, want.TenantID, got.TenantID)
	assert.Equal(t, want.ProjectID, got.ProjectID)
}

func TestSign_then_Verify_round_trips_session_epoch(t *testing.T) {
	s := newSigner(t)
	want := auth.Claims{
		PlayerID:     42,
		TenantID:     7,
		ProjectID:    9,
		SessionEpoch: 3,
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	tok, err := s.Sign(want)
	require.NoError(t, err)
	got, err := s.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, int64(3), got.SessionEpoch)
}

func TestVerify_rejects_modified_token(t *testing.T) {
	s := newSigner(t)
	tok, err := s.Sign(auth.Claims{PlayerID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	_, err = s.Verify(tok + "x")

	assert.Error(t, err)
}

func TestVerify_rejects_token_signed_with_different_key(t *testing.T) {
	a, _ := auth.NewSigner([]byte("key-a-padded-to-thirty-two-bytes!"))
	b, _ := auth.NewSigner([]byte("key-b-padded-to-thirty-two-bytes!"))

	tok, err := a.Sign(auth.Claims{PlayerID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	_, err = b.Verify(tok)

	assert.Error(t, err)
}

func TestVerify_rejects_token_with_unsupported_alg(t *testing.T) {
	s := newSigner(t)

	// "none" algorithm header (base64({"alg":"none","typ":"JWT"}).<empty payload>.<empty sig>)
	noneToken := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJzdWIiOjF9."
	_, err := s.Verify(noneToken)

	assert.Error(t, err)
}

func TestVerify_rejects_token_signed_with_hs512_alg_downgrade(t *testing.T) {
	s := newSigner(t)

	tok := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"puid": float64(1),
		"tid":  float64(1),
		"exp":  jwt.NewNumericDate(time.Now().Add(time.Hour)).Unix(),
	})
	signed, err := tok.SignedString(testKey)
	require.NoError(t, err)

	_, err = s.Verify(signed)

	assert.Error(t, err)
}

func TestVerify_rejects_token_without_exp(t *testing.T) {
	s := newSigner(t)
	signed := signRawHS256(t, testKey, jwt.MapClaims{
		"puid": float64(1),
		"tid":  float64(1),
	})

	_, err := s.Verify(signed)

	assert.Error(t, err)
}

func TestSign_returns_distinct_jti_per_call(t *testing.T) {
	s := newSigner(t)
	c := auth.Claims{PlayerID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)}

	tok1, err := s.Sign(c)
	require.NoError(t, err)
	tok2, err := s.Sign(c)
	require.NoError(t, err)

	assert.NotEqual(t, tok1, tok2, "two signed tokens for the same claims must differ via jti")
}

func TestNewSigner_with_random_key_works_when_no_key_provided(t *testing.T) {
	s, err := auth.NewSignerRandom()
	require.NoError(t, err)

	tok, err := s.Sign(auth.Claims{PlayerID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	got, err := s.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.PlayerID)
}

func TestVerify_returns_ErrTokenExpired_on_expired(t *testing.T) {
	s := newSigner(t)
	tok := signRawHS256(t, testKey, jwt.MapClaims{
		"puid": float64(1),
		"tid":  float64(1),
		"exp":  time.Now().Add(-time.Hour).Unix(),
	})

	_, err := s.Verify(tok)

	assert.True(t, errors.Is(err, auth.ErrTokenExpired))
}

func TestNewSignerFromHex_rejects_empty_input(t *testing.T) {
	_, err := auth.NewSignerFromHex("")

	assert.Error(t, err)
}

func TestNewSignerFromHex_rejects_invalid_hex(t *testing.T) {
	_, err := auth.NewSignerFromHex("zz-not-hex")

	assert.Error(t, err)
}

func TestNewSignerFromHex_rejects_short_decoded_key(t *testing.T) {
	// "deadbeef" decodes to 4 bytes, far below the 32-byte minimum.
	_, err := auth.NewSignerFromHex("deadbeef")

	assert.Error(t, err)
}

func TestVerify_rejects_token_signed_with_rs256(t *testing.T) {
	s := newSigner(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"puid": float64(1),
		"tid":  float64(1),
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	signed, err := tok.SignedString(rsaKey)
	require.NoError(t, err)

	_, err = s.Verify(signed)

	assert.Error(t, err)
}

func TestVerify_expired_token_with_wrong_key_is_not_ErrTokenExpired(t *testing.T) {
	s := newSigner(t)
	tok := signRawHS256(t, []byte("another-key-that-is-32-bytes-long!!!"), jwt.MapClaims{
		"puid": float64(1),
		"tid":  float64(1),
		"exp":  time.Now().Add(-time.Hour).Unix(),
	})

	_, err := s.Verify(tok)

	require.Error(t, err)
	assert.False(t, errors.Is(err, auth.ErrTokenExpired), "a forged token must not be reported as merely expired")
}

func TestSign_then_Verify_round_trips_expires_at(t *testing.T) {
	s := newSigner(t)
	want := time.Now().Add(time.Hour)

	tok, err := s.Sign(auth.Claims{PlayerID: 1, TenantID: 1, ExpiresAt: want})
	require.NoError(t, err)

	got, err := s.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, want.Unix(), got.ExpiresAt.Unix())
}

func TestSign_rejects_non_future_expiry(t *testing.T) {
	s := newSigner(t)
	cases := []struct {
		name      string
		expiresAt time.Time
	}{
		{name: "zero", expiresAt: time.Time{}},
		{name: "past", expiresAt: time.Now().Add(-time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Sign(auth.Claims{PlayerID: 1, TenantID: 1, ExpiresAt: tc.expiresAt})

			assert.Error(t, err)
		})
	}
}

func TestVerify_accepts_token_expired_within_leeway(t *testing.T) {
	s := newSigner(t)
	tok := signRawHS256(t, testKey, jwt.MapClaims{
		"puid": float64(1),
		"tid":  float64(1),
		"exp":  time.Now().Add(-5 * time.Second).Unix(),
	})

	_, err := s.Verify(tok)

	assert.NoError(t, err)
}
