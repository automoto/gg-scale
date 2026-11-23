package webutil_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/webutil"
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

func TestCrossOriginProtection_should_trust_the_public_base_url(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		origin  string
		want    int
	}{
		{"works_for_the_base_url_origin_with_another_host", "https://panel.example/", "https://panel.example", http.StatusNoContent},
		{"works_for_a_base_url_with_a_port_and_path", "http://10.0.0.5:8080/ggscale", "http://10.0.0.5:8080", http.StatusNoContent},
		{"refused_for_another_origin", "https://panel.example", "https://evil.example", http.StatusForbidden},
		{"refused_for_a_base_url_with_another_scheme", "https://panel.example", "http://panel.example", http.StatusForbidden},
		{"refused_with_no_base_url", "", "https://panel.example", http.StatusForbidden},
		{"refused_with_a_bad_base_url", "ftp://panel.example", "ftp://panel.example", http.StatusForbidden},
		{"refused_with_a_base_url_that_does_not_parse", "https://panel.example:bad", "https://panel.example", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := webutil.CrossOriginProtection(tc.baseURL)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			req := httptest.NewRequest(http.MethodPost, "http://ggscale:8080/v1/control-panel/login", nil)
			req.Header.Set("Origin", tc.origin)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			assert.Equal(t, tc.want, rec.Code)
		})
	}
}
