package players

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/twofactor"
)

const testTwoFactorHexKey = "6368616e676520746869732070617373776f726420746f206120736563726574"

func newTwoFactorTestHandler(t *testing.T) *Handler {
	t.Helper()
	cipher, err := twofactor.NewCipher(testTwoFactorHexKey)
	require.NoError(t, err)
	return &Handler{twoFactor: cipher, now: time.Now}
}

func requestWithCookie(name, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, accountChallengePath, nil)
	r.AddCookie(&http.Cookie{Name: name, Value: value})
	return r
}

func TestAccountTwoFactorCookie_roundtrip(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	accountID := uuid.New()
	rec := httptest.NewRecorder()
	h.setAccountTwoFactorCookie(rec, accountID, "player@example.com")
	cookie := rec.Result().Cookies()[0]
	require.Equal(t, accountTwoFactorCookieName, cookie.Name)
	assert.True(t, cookie.HttpOnly)

	gotID, gotEmail, ok := h.accountTwoFactorPending(requestWithCookie(cookie.Name, cookie.Value))

	require.True(t, ok)
	assert.Equal(t, accountID, gotID)
	assert.Equal(t, "player@example.com", gotEmail)
}

func TestAccountTwoFactorCookie_rejects_tampered_value(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	rec := httptest.NewRecorder()
	h.setAccountTwoFactorCookie(rec, uuid.New(), "player@example.com")
	cookie := rec.Result().Cookies()[0]

	_, _, ok := h.accountTwoFactorPending(requestWithCookie(cookie.Name, "x"+cookie.Value[1:]))

	assert.False(t, ok)
}

func TestAccountTwoFactorCookie_rejects_expired(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	rec := httptest.NewRecorder()
	h.setAccountTwoFactorCookie(rec, uuid.New(), "player@example.com")
	cookie := rec.Result().Cookies()[0]
	h.now = func() time.Time { return time.Now().Add(twofactor.PendingTTL + time.Minute) }

	_, _, ok := h.accountTwoFactorPending(requestWithCookie(cookie.Name, cookie.Value))

	assert.False(t, ok)
}

func TestAccountTwoFactorCookie_rejects_non_uuid_subject(t *testing.T) {
	// A dashboard pending payload (int64 subject) signed with the same key
	// must not open as a player pending grant.
	h := newTwoFactorTestHandler(t)
	value := twofactor.EncodePending(h.twoFactor.PendingKey(), twofactor.Pending{
		Subject:   "42",
		Email:     "op@example.com",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})

	_, _, ok := h.accountTwoFactorPending(requestWithCookie(accountTwoFactorCookieName, value))

	assert.False(t, ok)
}

func TestAccountTwoFactorCookie_requires_cipher(t *testing.T) {
	h := newTwoFactorTestHandler(t)
	rec := httptest.NewRecorder()
	h.setAccountTwoFactorCookie(rec, uuid.New(), "player@example.com")
	cookie := rec.Result().Cookies()[0]
	h.twoFactor = nil

	_, _, ok := h.accountTwoFactorPending(requestWithCookie(cookie.Name, cookie.Value))

	assert.False(t, ok)
}
