package webutil_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/webutil"
)

func TestSecurityHeadersDisallowInlineControlPanelAssets(t *testing.T) {
	h := webutil.SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	h.ServeHTTP(w, r)

	csp := w.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'self'")
	assert.Contains(t, csp, "style-src 'self'")
	assert.Contains(t, csp, "style-src-attr 'none'")
	assert.Contains(t, csp, "script-src 'self'")
	assert.Contains(t, csp, "script-src-attr 'none'")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.Contains(t, csp, "object-src 'none'")
	assert.NotContains(t, csp, "unsafe-inline")
}

func TestPlayerSecurityHeadersAllowFirstPartyStylesBlockScripts(t *testing.T) {
	h := webutil.PlayerSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	h.ServeHTTP(w, r)

	csp := w.Header().Get("Content-Security-Policy")
	// Everything not explicitly allowed (frames, media, manifests, ...) stays
	// blocked, so an HTML-injection bug can't embed same-origin content.
	assert.Contains(t, csp, "default-src 'none'")
	// Player pages load first-party stylesheets (Pico + the shared sheet),
	// which need fonts and data: SVG backgrounds...
	assert.Contains(t, csp, "style-src 'self'")
	assert.Contains(t, csp, "style-src-attr 'none'")
	assert.Contains(t, csp, "font-src 'self'")
	assert.Contains(t, csp, "img-src 'self' data:")
	// ...but run no script at all.
	assert.Contains(t, csp, "script-src 'none'")
	assert.Contains(t, csp, "script-src-attr 'none'")
	assert.Contains(t, csp, "form-action 'self'")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.NotContains(t, csp, "unsafe-inline")
}

func TestSecurityHeadersWithFormActions_should_extend_only_form_action(t *testing.T) {
	tests := []struct {
		name       string
		middleware func(http.Handler) http.Handler
		base       func(http.Handler) http.Handler
	}{
		{"control panel", webutil.SecurityHeadersWithFormActions("https://accounts.google.com", "https://github.com"), webutil.SecurityHeaders},
		{"player", webutil.PlayerSecurityHeadersWithFormActions("https://accounts.google.com", "https://github.com"), webutil.PlayerSecurityHeaders},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
			got := httptest.NewRecorder()
			base := httptest.NewRecorder()

			tc.middleware(next).ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/", nil))
			tc.base(next).ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/", nil))

			csp := got.Header().Get("Content-Security-Policy")
			// Assert the literal. Re-deriving want with the same
			// strings.Replace the code uses makes both sides no-op
			// together if the base policy stops spelling
			// "form-action 'self'" verbatim: the origins drop out, the
			// browser blocks the provider redirect, and the test stays green.
			const origins = " https://accounts.google.com https://github.com"
			assert.Contains(t, csp, "form-action 'self'"+origins)
			// Nothing else moved: strip the origins back out and the base
			// policy must come back byte for byte.
			assert.Equal(t, base.Header().Get("Content-Security-Policy"), strings.Replace(csp, origins, "", 1))
		})
	}
}

func TestSecurityHeadersWithFormActions_without_origins_is_the_base_policy(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {})
	got := httptest.NewRecorder()
	base := httptest.NewRecorder()

	webutil.PlayerSecurityHeadersWithFormActions()(next).ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/", nil))
	webutil.PlayerSecurityHeaders(next).ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, base.Header().Get("Content-Security-Policy"), got.Header().Get("Content-Security-Policy"))
}

func TestSanitizeHeaderAcceptsPlainText(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"ascii", "Welcome to ggscale"},
		{"utf8", "Привет, мир"},
		{"tab is allowed", "Subject:\tindented"},
		{"max length minus one", strings.Repeat("a", webutil.MaxHeaderLineBytes-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := webutil.SanitizeHeader(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.in, out)
		})
	}
}

func TestSanitizeHeaderRejectsControlChars(t *testing.T) {
	tests := []struct {
		name string
		in   string
		err  error
	}{
		{"CR", "evil\rBcc: a@b", webutil.ErrHeaderInjection},
		{"LF", "evil\nBcc: a@b", webutil.ErrHeaderInjection},
		{"CRLF", "evil\r\nBcc: a@b", webutil.ErrHeaderInjection},
		{"NUL", "evil\x00", webutil.ErrHeaderInjection},
		{"vertical tab", "evil\v", webutil.ErrHeaderInjection},
		{"backspace", "evil\b", webutil.ErrHeaderInjection},
		{"DEL", "evil\x7f", webutil.ErrHeaderInjection},
		{"too long", strings.Repeat("x", webutil.MaxHeaderLineBytes+1), webutil.ErrHeaderTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := webutil.SanitizeHeader(tt.in)
			assert.ErrorIs(t, err, tt.err)
		})
	}
}
