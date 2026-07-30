package webutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailUnsubscribeToken_round_trips(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	token := EmailUnsubscribeToken(key, "player@example.com")

	email, ok := ParseEmailUnsubscribeToken(key, token)

	require.True(t, ok)
	assert.Equal(t, "player@example.com", email)
}

func TestParseEmailUnsubscribeToken_rejects_forgeries(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	token := EmailUnsubscribeToken(key, "player@example.com")

	_, ok := ParseEmailUnsubscribeToken(key, token+"x")
	assert.False(t, ok, "tampered mac must be rejected")

	_, ok = ParseEmailUnsubscribeToken([]byte("another-key-another-key-another!"), token)
	assert.False(t, ok, "a token minted under a different key must be rejected")

	_, ok = ParseEmailUnsubscribeToken(key, "not-a-token")
	assert.False(t, ok)
}
