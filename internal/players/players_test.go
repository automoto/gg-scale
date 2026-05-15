package players

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidEmail(t *testing.T) {
	tests := []struct {
		email string
		ok    bool
	}{
		{"a@b.c", true},
		{"user@example.com", true},
		{"", false},
		{"noat", false},
		{"@nodomain.com", false},
		{"user@", false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.ok, validEmail(tc.email), "email=%q", tc.email)
	}
}

func TestEncodeDecodeVerifyCookie_roundtrip(t *testing.T) {
	key := []byte("test-signing-key-32-bytes-long-aa")
	p := verifyCookiePayload{EndUserID: 99, ProjectID: 7, Email: "p@example.com"}
	enc := encodeVerifyCookie(p, key)
	require.NotEmpty(t, enc)
	out, ok := decodeVerifyCookie(enc, key)
	require.True(t, ok)
	assert.Equal(t, p, out)
}

func TestDecodeVerifyCookie_rejects_tampered_payload(t *testing.T) {
	key := []byte("k")
	enc := encodeVerifyCookie(verifyCookiePayload{EndUserID: 1, ProjectID: 1, Email: "a@b.c"}, key)
	bad := "x" + enc[1:]
	_, ok := decodeVerifyCookie(bad, key)
	assert.False(t, ok)
}

func TestDecodeVerifyCookie_rejects_wrong_key(t *testing.T) {
	enc := encodeVerifyCookie(verifyCookiePayload{EndUserID: 1, ProjectID: 1, Email: "a@b.c"}, []byte("kA"))
	_, ok := decodeVerifyCookie(enc, []byte("kB"))
	assert.False(t, ok)
}

func TestDecodeVerifyCookie_rejects_garbage(t *testing.T) {
	for _, raw := range []string{"", "no-dot", "..", "ZZ?"} {
		_, ok := decodeVerifyCookie(raw, []byte("k"))
		assert.False(t, ok, "raw=%q", raw)
	}
}

func TestPathHelpers(t *testing.T) {
	assert.Equal(t, "/v1/players/p/42/login", playerLoginPath(42))
	assert.Equal(t, "/v1/players/p/42/verify", playerVerifyPath(42))
	assert.Equal(t, "/v1/players/p/42/account", playerAccountPath(42))
}
