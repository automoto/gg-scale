package webutil_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/webutil"
)

func TestSecurityHeadersDisallowInlineDashboardAssets(t *testing.T) {
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

func TestPlayerSecurityHeadersDisallowScriptsAndStyles(t *testing.T) {
	h := webutil.PlayerSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	h.ServeHTTP(w, r)

	csp := w.Header().Get("Content-Security-Policy")
	assert.Contains(t, csp, "default-src 'none'")
	assert.Contains(t, csp, "style-src 'none'")
	assert.Contains(t, csp, "style-src-attr 'none'")
	assert.Contains(t, csp, "script-src 'none'")
	assert.Contains(t, csp, "script-src-attr 'none'")
	assert.Contains(t, csp, "form-action 'self'")
	assert.Contains(t, csp, "frame-ancestors 'none'")
	assert.NotContains(t, csp, "unsafe-inline")
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
