package controlpanel

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerifyPendingCookieSurvivesHandlerReconstruction(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	first := newHandler(Deps{VerifySigningKey: key})
	second := newHandler(Deps{VerifySigningKey: key})
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	payload := verifyPendingPayload{UserID: 42, Email: "alice@example.com"}
	recorder := httptest.NewRecorder()

	first.setVerifyPendingCookie(recorder, payload)
	request := httptest.NewRequest("GET", "/verify", nil)
	request.AddCookie(recorder.Result().Cookies()[0])
	got, ok := second.verifyPendingFromCookie(request)

	require.True(t, ok)
	assert.Equal(t, payload.UserID, got.UserID)
}

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
