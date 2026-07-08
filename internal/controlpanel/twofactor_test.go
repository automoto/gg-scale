package controlpanel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/twofactor"
)

const testTwoFactorHexKey = "6368616e676520746869732070617373776f726420746f206120736563726574"

func newTwoFactorTestHandler(t *testing.T) *Handler {
	t.Helper()
	cipher, err := twofactor.NewCipher(testTwoFactorHexKey)
	require.NoError(t, err)
	return &Handler{
		twoFactor:        cipher,
		now:              time.Now,
		verifySigningKey: []byte("test-key-32-bytes-long-aaaaaaaaa"),
	}
}

func requestWithCookie(name, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, pathControlPanelLogin2FA, nil)
	r.AddCookie(&http.Cookie{Name: name, Value: value})
	return r
}

func TestTwoFactorPendingCookie_roundtrip(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	rec := httptest.NewRecorder()
	h.setTwoFactorPendingCookie(rec, controlPanelUser{ID: 42, Email: "op@example.com"})
	cookie := rec.Result().Cookies()[0]
	require.Equal(t, twoFactorPendingCookieName, cookie.Name)
	assert.True(t, cookie.HttpOnly)

	user, ok := h.twoFactorPendingFromRequest(requestWithCookie(cookie.Name, cookie.Value))

	require.True(t, ok)
	assert.Equal(t, int64(42), user.ID)
	assert.Equal(t, "op@example.com", user.Email)
}

func TestTwoFactorPendingCookie_rejects_tampered_value(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	rec := httptest.NewRecorder()
	h.setTwoFactorPendingCookie(rec, controlPanelUser{ID: 42, Email: "op@example.com"})
	cookie := rec.Result().Cookies()[0]

	_, ok := h.twoFactorPendingFromRequest(requestWithCookie(cookie.Name, "x"+cookie.Value[1:]))

	assert.False(t, ok)
}

func TestTwoFactorPendingCookie_rejects_expired(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	rec := httptest.NewRecorder()
	h.setTwoFactorPendingCookie(rec, controlPanelUser{ID: 42, Email: "op@example.com"})
	cookie := rec.Result().Cookies()[0]
	h.now = func() time.Time { return time.Now().Add(twofactor.PendingTTL + time.Minute) }

	_, ok := h.twoFactorPendingFromRequest(requestWithCookie(cookie.Name, cookie.Value))

	assert.False(t, ok)
}

func TestTwoFactorPendingCookie_rejects_verify_cookie_replay(t *testing.T) {
	// An email-verify pending cookie pasted into the 2FA cookie slot must
	// not open: different key (per-process vs HKDF-derived) and different
	// payload shape.
	h := newTwoFactorTestHandler(t)
	verifyValue := encodeVerifyCookie(verifyPendingPayload{
		UserID:    42,
		Email:     "op@example.com",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}, h.verifyCookieKey())

	_, ok := h.twoFactorPendingFromRequest(requestWithCookie(twoFactorPendingCookieName, verifyValue))

	assert.False(t, ok)
}

func TestTwoFactorPendingCookie_requires_cipher(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	rec := httptest.NewRecorder()
	h.setTwoFactorPendingCookie(rec, controlPanelUser{ID: 42, Email: "op@example.com"})
	cookie := rec.Result().Cookies()[0]
	h.twoFactor = nil

	_, ok := h.twoFactorPendingFromRequest(requestWithCookie(cookie.Name, cookie.Value))

	assert.False(t, ok)
}
