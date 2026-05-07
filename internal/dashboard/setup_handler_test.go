package dashboard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newSetupHandler(b *Bootstrap) *Handler {
	return &Handler{bootstrap: b}
}

func postForm(values url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestSetupTokenPage_ShowsFilePathFromBootstrap(t *testing.T) {
	h := newSetupHandler(NewBootstrap("tok", "/tmp/p"))
	w := httptest.NewRecorder()
	h.setupTokenPage(w, httptest.NewRequest(http.MethodGet, "/v1/dashboard/setup", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "/tmp/p")
	assert.Contains(t, w.Body.String(), `action="/v1/dashboard/setup/token"`)
}

func TestSetupTokenPage_GoneWhenBootstrapNotPending(t *testing.T) {
	h := newSetupHandler(DisabledBootstrap())
	w := httptest.NewRecorder()
	h.setupTokenPage(w, httptest.NewRequest(http.MethodGet, "/v1/dashboard/setup", nil))

	assert.Equal(t, http.StatusGone, w.Code)
}

func TestVerifySetupToken_RejectsWrongToken(t *testing.T) {
	h := newSetupHandler(NewBootstrap("good", "/p"))
	w := httptest.NewRecorder()
	h.verifySetupToken(w, postForm(url.Values{"bootstrap_token": {"bad"}}))

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `action="/v1/dashboard/setup/token"`)
	assert.Contains(t, body, "Invalid bootstrap token")
}

func TestVerifySetupToken_RendersStep2OnGoodToken(t *testing.T) {
	h := newSetupHandler(NewBootstrap("good", "/p"))
	w := httptest.NewRecorder()
	h.verifySetupToken(w, postForm(url.Values{"bootstrap_token": {"good"}}))

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<input type="hidden" name="bootstrap_token" value="good"`)
	assert.Contains(t, body, `action="/v1/dashboard/setup"`)
}

func TestVerifySetupToken_GoneAfterCompletion(t *testing.T) {
	b := NewBootstrap("t", "")
	b.complete()
	h := newSetupHandler(b)
	w := httptest.NewRecorder()
	h.verifySetupToken(w, postForm(url.Values{"bootstrap_token": {"t"}}))

	assert.Equal(t, http.StatusGone, w.Code)
}

func TestCompleteSetup_FallsBackToStep1WhenTokenInvalid(t *testing.T) {
	h := newSetupHandler(NewBootstrap("good", "/p"))
	w := httptest.NewRecorder()
	h.completeSetup(w, postForm(url.Values{
		"bootstrap_token": {"tampered"},
		"email":           {"admin@example.com"},
		"password":        {"long-enough-password"},
	}))

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `action="/v1/dashboard/setup/token"`)
	assert.Contains(t, body, "Bootstrap token no longer valid")
}

func TestCompleteSetup_RejectsInvalidEmailOrPassword(t *testing.T) {
	cases := []struct {
		name        string
		email       string
		password    string
		wantEmail   bool
		wantPasswrd bool
	}{
		{"short password", "admin@example.com", "short", false, true},
		{"bad email", "not-an-email", "long-enough-password", true, false},
		{"both invalid", "not-an-email", "short", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newSetupHandler(NewBootstrap("good", "/p"))
			w := httptest.NewRecorder()
			h.completeSetup(w, postForm(url.Values{
				"bootstrap_token": {"good"},
				"email":           {tc.email},
				"password":        {tc.password},
			}))

			assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
			body := w.Body.String()
			assert.Contains(t, body, `action="/v1/dashboard/setup"`)
			if tc.wantEmail {
				assert.Contains(t, body, "Enter a valid email address")
			}
			if tc.wantPasswrd {
				assert.Contains(t, body, "Password must be at least 12 characters")
			}
		})
	}
}
