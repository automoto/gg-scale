package webutil_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/webutil"
)

func TestCSRFCookieMintsOnSafeMethod(t *testing.T) {
	mw := webutil.CSRFCookie(webutil.CSRFConfig{Path: "/"})
	var ctxToken string
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctxToken = webutil.CSRFTokenFromContext(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, webutil.CSRFCookieName, cookies[0].Name)
	assert.NotEmpty(t, cookies[0].Value)
	assert.Equal(t, cookies[0].Value, ctxToken)
}

func TestRequireCSRFAcceptsMatchingFormField(t *testing.T) {
	h := webutil.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	form := url.Values{webutil.CSRFFormField: {"abc"}}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: webutil.CSRFCookieName, Value: "abc"})
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestRequireCSRFRejectsMismatch(t *testing.T) {
	h := webutil.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	form := url.Values{webutil.CSRFFormField: {"wrong"}}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: webutil.CSRFCookieName, Value: "right"})
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRequireCSRFRejectsMissingCookie(t *testing.T) {
	h := webutil.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRequireCSRFPassesSafeMethods(t *testing.T) {
	h := webutil.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	assert.Equal(t, http.StatusOK, w.Code)
}
