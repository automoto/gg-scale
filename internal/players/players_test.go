package players

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/webassets"
)

var playerTestVerifySigningKey = []byte("0123456789abcdef0123456789abcdef")

func TestAccountVerifyCookieSurvivesHandlerReconstruction(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	first := newHandler(Deps{VerifySigningKey: key})
	second := newHandler(Deps{VerifySigningKey: key})
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	accountID := uuid.New()
	recorder := httptest.NewRecorder()

	first.setAccountVerifyCookie(recorder, accountID, "player@example.com")
	request := httptest.NewRequest(http.MethodGet, "/account/verify", nil)
	request.AddCookie(recorder.Result().Cookies()[0])
	got, ok := second.accountVerifyCookie(request)

	require.True(t, ok)
	assert.Equal(t, accountID, got.AccountID)
}

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
	p := verifyCookiePayload{PlayerID: 99, ProjectID: 7, Email: "p@example.com"}
	enc := encodeVerifyCookie(p, key)
	require.NotEmpty(t, enc)
	out, ok := decodeVerifyCookie(enc, key)
	require.True(t, ok)
	assert.Equal(t, p, out)
}

func TestDecodeVerifyCookie_rejects_tampered_payload(t *testing.T) {
	key := []byte("k")
	enc := encodeVerifyCookie(verifyCookiePayload{PlayerID: 1, ProjectID: 1, Email: "a@b.c"}, key)
	bad := "x" + enc[1:]
	_, ok := decodeVerifyCookie(bad, key)
	assert.False(t, ok)
}

func TestDecodeVerifyCookie_rejects_wrong_key(t *testing.T) {
	enc := encodeVerifyCookie(verifyCookiePayload{PlayerID: 1, ProjectID: 1, Email: "a@b.c"}, []byte("kA"))
	_, ok := decodeVerifyCookie(enc, []byte("kB"))
	assert.False(t, ok)
}

func TestDecodeVerifyCookie_rejects_garbage(t *testing.T) {
	for _, raw := range []string{"", "no-dot", "..", "ZZ?"} {
		_, ok := decodeVerifyCookie(raw, []byte("k"))
		assert.False(t, ok, "raw=%q", raw)
	}
}

func TestPlayerPagesAllowFirstPartyStylesButNoScripts(t *testing.T) {
	h := New(Deps{Config: Config{Mount: true}, VerifySigningKey: playerTestVerifySigningKey})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/account/login", nil)

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	csp := rec.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "style-src 'self'")
	assert.Contains(t, csp, "font-src 'self'")
	assert.Contains(t, csp, "img-src 'self' data:")
	assert.Contains(t, csp, "script-src 'none'")
	assert.NotContains(t, csp, "unsafe-inline")
}

func TestPlayerPagesLinkSharedStylesheetsAndRunNoScript(t *testing.T) {
	h := New(Deps{Config: Config{Mount: true}, VerifySigningKey: playerTestVerifySigningKey})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/account/login", nil)

	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	html := rec.Body.String()
	assert.Contains(t, html, `data-theme="dark"`)
	// Stylesheet links carry the cache-busting content version.
	assert.Contains(t, html, `href="`+webassets.URL("pico.min.css")+`"`)
	assert.Contains(t, html, `href="`+webassets.URL("app.css")+`"`)
	// Player pages remain script-free.
	assert.NotContains(t, html, "<script")
	assert.NotContains(t, html, "<style")
}
