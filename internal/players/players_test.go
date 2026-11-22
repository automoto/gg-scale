package players

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/webassets"
	"github.com/automoto/gg-scale/internal/webutil"
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

func newRateLimitedPlayerSite() http.Handler {
	return New(Deps{
		Config:           Config{Mount: true},
		VerifySigningKey: playerTestVerifySigningKey,
		Limiter:          ratelimit.NewCacheLimiter(memory.New()),
		Registry:         prometheus.NewRegistry(),
	})
}

func TestPlayerSite_should_not_throttle_page_gets_above_auth_ip_burst(t *testing.T) {
	h := newRateLimitedPlayerSite()
	pages := []string{"/account/", "/account/friends", "/account/2fa", "/account/remote-addrs", "/account/login", "/account/signup"}

	statuses := map[int]int{}
	for range int(ratelimit.AuthIPBurst) + 1 {
		for _, page := range pages {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, page, nil))
			statuses[rec.Code]++
		}
	}

	assert.Zero(t, statuses[http.StatusTooManyRequests])
}

// signupBurst sends one more invalid signup than the auth bucket holds, all
// from one IP, and returns the last response. Each invalid signup stops at
// form validation, before any database work.
func signupBurst(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	h := newRateLimitedPlayerSite()
	form := url.Values{webutil.CSRFFormField: {"tok"}, "email": {"not-an-email"}}
	var rec *httptest.ResponseRecorder
	for i := range int(ratelimit.AuthIPBurst) + 1 {
		req := httptest.NewRequest(http.MethodPost, "/account/signup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: webutil.CSRFCookieName, Value: "tok"})
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if i < int(ratelimit.AuthIPBurst) {
			require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "request %d", i+1)
		}
	}
	return rec
}

func TestPlayerSite_should_throttle_credential_post_above_auth_ip_burst(t *testing.T) {
	rec := signupBurst(t)

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestPlayerSite_should_set_retry_after_on_throttled_credential_post(t *testing.T) {
	rec := signupBurst(t)

	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
}

// TestPlayerSite_should_refuse_a_cross_site_write_on_every_route walks every
// state-changing route, so a new route cannot miss the cross-origin check.
func TestPlayerSite_should_refuse_a_cross_site_write_on_every_route(t *testing.T) {
	h := New(Deps{Config: Config{Mount: true}, VerifySigningKey: playerTestVerifySigningKey})
	routes, ok := h.(chi.Routes)
	require.True(t, ok)
	param := regexp.MustCompile(`\{[^}]+\}`)

	refused := map[string]bool{}
	want := map[string]bool{}
	require.NoError(t, chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return nil
		}
		req := httptest.NewRequest(method, param.ReplaceAllString(route, "1"), nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", "https://evil.example")
		refused[route] = refusedAsCrossOrigin(h, req)
		want[route] = true
		return nil
	}))

	require.NotEmpty(t, refused)
	assert.Equal(t, want, refused)
}

// refusedAsCrossOrigin reports whether the cross-origin check answered the
// request. A handler that panics (it ran with no database) did not.
func refusedAsCrossOrigin(h http.Handler, req *http.Request) (refused bool) {
	defer func() {
		if recover() != nil {
			refused = false
		}
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "cross-origin request detected")
}

func TestPlayerSite_should_trust_a_post_from_the_base_url_origin(t *testing.T) {
	h := New(Deps{Config: Config{Mount: true, BaseURL: "https://play.example"}, VerifySigningKey: playerTestVerifySigningKey})
	req := httptest.NewRequest(http.MethodPost, "http://ggscale:8080/account/signup", nil)
	req.Header.Set("Origin", "https://play.example")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	// The request passed the origin check and reached the CSRF check.
	assert.Equal(t, "missing csrf cookie\n", rec.Body.String())
}
