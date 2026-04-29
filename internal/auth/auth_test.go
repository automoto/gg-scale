package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/auth"
)

func newSigner(t *testing.T) *auth.Signer {
	t.Helper()
	s, err := auth.NewSigner([]byte("test-key-must-be-at-least-32-bytes-long"))
	require.NoError(t, err)
	return s
}

func TestNewSigner_rejects_short_keys(t *testing.T) {
	_, err := auth.NewSigner([]byte("too-short"))

	assert.Error(t, err)
}

func TestSign_then_Verify_round_trips_claims(t *testing.T) {
	s := newSigner(t)
	want := auth.Claims{
		EndUserID: 42,
		TenantID:  7,
		ProjectID: 9,
		ExpiresAt: time.Now().Add(time.Hour),
	}

	tok, err := s.Sign(want)
	require.NoError(t, err)

	got, err := s.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, want.EndUserID, got.EndUserID)
	assert.Equal(t, want.TenantID, got.TenantID)
	assert.Equal(t, want.ProjectID, got.ProjectID)
}

func TestVerify_rejects_modified_token(t *testing.T) {
	s := newSigner(t)
	tok, err := s.Sign(auth.Claims{EndUserID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	_, err = s.Verify(tok + "x")

	assert.Error(t, err)
}

func TestVerify_rejects_expired_token(t *testing.T) {
	s := newSigner(t)
	tok, err := s.Sign(auth.Claims{EndUserID: 1, TenantID: 1, ExpiresAt: time.Now().Add(-time.Hour)})
	require.NoError(t, err)

	_, err = s.Verify(tok)

	assert.Error(t, err)
}

func TestVerify_rejects_token_signed_with_different_key(t *testing.T) {
	a, _ := auth.NewSigner([]byte("key-a-padded-to-thirty-two-bytes!"))
	b, _ := auth.NewSigner([]byte("key-b-padded-to-thirty-two-bytes!"))

	tok, err := a.Sign(auth.Claims{EndUserID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
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
	key := []byte("test-key-must-be-at-least-32-bytes-long")

	tok := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{
		"euid": float64(1),
		"tid":  float64(1),
		"exp":  jwt.NewNumericDate(time.Now().Add(time.Hour)).Unix(),
	})
	signed, err := tok.SignedString(key)
	require.NoError(t, err)

	_, err = s.Verify(signed)

	assert.Error(t, err)
}

func TestSign_returns_distinct_jti_per_call(t *testing.T) {
	s := newSigner(t)
	c := auth.Claims{EndUserID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)}

	tok1, err := s.Sign(c)
	require.NoError(t, err)
	tok2, err := s.Sign(c)
	require.NoError(t, err)

	assert.NotEqual(t, tok1, tok2, "two signed tokens for the same claims must differ via jti")
}

func TestNewSigner_with_random_key_works_when_no_key_provided(t *testing.T) {
	s, err := auth.NewSignerRandom()
	require.NoError(t, err)

	tok, err := s.Sign(auth.Claims{EndUserID: 1, TenantID: 1, ExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	got, err := s.Verify(tok)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.EndUserID)
}

func TestVerify_returns_ErrTokenExpired_on_expired(t *testing.T) {
	s := newSigner(t)
	tok, err := s.Sign(auth.Claims{EndUserID: 1, TenantID: 1, ExpiresAt: time.Now().Add(-time.Hour)})
	require.NoError(t, err)

	_, err = s.Verify(tok)

	assert.True(t, errors.Is(err, auth.ErrTokenExpired))
}
