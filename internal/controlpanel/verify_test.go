package controlpanel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeVerifyCookie_roundtrip(t *testing.T) {
	key := []byte("test-key-32-bytes-long-aaaaaaaaa")
	p := verifyPendingPayload{UserID: 42, Email: "alice@example.com"}
	got := encodeVerifyCookie(p, key)
	require.NotEmpty(t, got)
	out, ok := decodeVerifyCookie(got, key)
	require.True(t, ok)
	assert.Equal(t, p, out)
}

func TestDecodeVerifyCookie_rejects_tampered_payload(t *testing.T) {
	key := []byte("test-key")
	enc := encodeVerifyCookie(verifyPendingPayload{UserID: 1, Email: "a@b.com"}, key)
	// Flip a byte in the payload half.
	bad := "x" + enc[1:]
	_, ok := decodeVerifyCookie(bad, key)
	assert.False(t, ok)
}

func TestDecodeVerifyCookie_rejects_tampered_signature(t *testing.T) {
	key := []byte("test-key")
	enc := encodeVerifyCookie(verifyPendingPayload{UserID: 1, Email: "a@b.com"}, key)
	bad := enc[:len(enc)-2] + "AA"
	_, ok := decodeVerifyCookie(bad, key)
	assert.False(t, ok)
}

func TestDecodeVerifyCookie_rejects_wrong_key(t *testing.T) {
	enc := encodeVerifyCookie(verifyPendingPayload{UserID: 1, Email: "a@b.com"}, []byte("kA"))
	_, ok := decodeVerifyCookie(enc, []byte("kB"))
	assert.False(t, ok)
}

func TestDecodeVerifyCookie_rejects_garbage(t *testing.T) {
	tests := []string{"", "no-dot", "..", "ZZZ.AAA", "validb64.??"}
	for _, raw := range tests {
		_, ok := decodeVerifyCookie(raw, []byte("k"))
		assert.False(t, ok, "raw=%q", raw)
	}
}
