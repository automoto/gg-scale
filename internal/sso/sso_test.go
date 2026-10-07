package sso_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/sso"
	"github.com/automoto/gg-scale/internal/sso/ssotest"
)

const (
	testCookie   = "sso_state"
	testCallback = "http://app.test/cb"
	testProfile  = `{"sub":"g-123","email":"p@example.com","email_verified":true,"name":"Pat"}`
)

func newFlow(t *testing.T, fake *ssotest.Fake) *sso.Flow {
	t.Helper()
	google := sso.Google("client-id", "client-secret", testCallback)
	google.OAuth.Endpoint = fake.Endpoint()
	google.ProfileURL = fake.ProfileURL()
	return &sso.Flow{
		Providers:  map[string]sso.Provider{"google": google},
		Key:        sso.StateKey([]byte("0123456789abcdef0123456789abcdef")),
		Purpose:    "test",
		CookieName: testCookie,
		CookiePath: "/cb",
		Now:        time.Now,
	}
}

// start runs Flow.Start and the provider's authorize step, and returns the
// state cookie with the callback request the provider sends the browser to.
func start(t *testing.T, flow *sso.Flow, mode, actor, code string) (*http.Cookie, *http.Request) {
	t.Helper()
	rec := httptest.NewRecorder()
	require.NoError(t, flow.Start(rec, httptest.NewRequest(http.MethodPost, "/start", nil), "google", mode, actor, code))
	require.Equal(t, http.StatusSeeOther, rec.Code)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)

	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Get(rec.Header().Get("Location"))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusFound, resp.StatusCode)
	callback := httptest.NewRequest(http.MethodGet, resp.Header.Get("Location"), nil)
	callback.AddCookie(cookies[0])
	return cookies[0], callback
}

func withQuery(t *testing.T, r *http.Request, key, value string) *http.Request {
	t.Helper()
	q := r.URL.Query()
	q.Set(key, value)
	out := httptest.NewRequest(http.MethodGet, r.URL.Path+"?"+q.Encode(), nil)
	for _, c := range r.Cookies() {
		out.AddCookie(c)
	}
	return out
}

func TestFlow_should_return_identity_and_pending_on_good_round_trip(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetProfile(testProfile)
	flow := newFlow(t, fake)
	_, callback := start(t, flow, sso.ModeInvite, "42", "invite-code")

	pending, id, err := flow.Finish(httptest.NewRecorder(), callback, "google")

	require.NoError(t, err)
	assert.Equal(t, sso.Identity{Provider: "google", Subject: "g-123", Email: "p@example.com", EmailVerified: true, Name: "Pat"}, id)
	assert.Equal(t, sso.ModeInvite, pending.Mode)
	assert.Equal(t, "42", pending.Actor)
	assert.Equal(t, "invite-code", pending.Code)
}

func TestFlow_should_delete_state_cookie_on_finish(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetProfile(testProfile)
	flow := newFlow(t, fake)
	_, callback := start(t, flow, sso.ModeSignIn, "", "")
	rec := httptest.NewRecorder()

	_, _, err := flow.Finish(rec, callback, "google")

	require.NoError(t, err)
	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, testCookie, cookies[0].Name)
	assert.Equal(t, -1, cookies[0].MaxAge)
}

func TestFlow_should_send_pkce_challenge_to_provider(t *testing.T) {
	flow := newFlow(t, ssotest.New(t))
	rec := httptest.NewRecorder()

	require.NoError(t, flow.Start(rec, httptest.NewRequest(http.MethodPost, "/start", nil), "google", sso.ModeSignIn, "", ""))

	location, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "S256", location.Query().Get("code_challenge_method"))
	assert.NotEmpty(t, location.Query().Get("code_challenge"))
	assert.Equal(t, testCallback, location.Query().Get("redirect_uri"))
}

func TestFlow_should_reject_bad_state(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, flow *sso.Flow, cookie *http.Cookie, callback *http.Request) (*sso.Flow, *http.Request)
	}{
		{"state does not match", func(t *testing.T, flow *sso.Flow, _ *http.Cookie, cb *http.Request) (*sso.Flow, *http.Request) {
			return flow, withQuery(t, cb, "state", "forged")
		}},
		{"no cookie", func(_ *testing.T, flow *sso.Flow, _ *http.Cookie, cb *http.Request) (*sso.Flow, *http.Request) {
			return flow, httptest.NewRequest(http.MethodGet, cb.URL.String(), nil)
		}},
		{"tampered cookie", func(_ *testing.T, flow *sso.Flow, c *http.Cookie, cb *http.Request) (*sso.Flow, *http.Request) {
			out := httptest.NewRequest(http.MethodGet, cb.URL.String(), nil)
			out.AddCookie(&http.Cookie{Name: c.Name, Value: "x" + c.Value})
			return flow, out
		}},
		{"expired cookie", func(_ *testing.T, flow *sso.Flow, _ *http.Cookie, cb *http.Request) (*sso.Flow, *http.Request) {
			later := *flow
			later.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
			return &later, cb
		}},
		{"wrong purpose", func(_ *testing.T, flow *sso.Flow, _ *http.Cookie, cb *http.Request) (*sso.Flow, *http.Request) {
			other := *flow
			other.Purpose = "other-surface"
			return &other, cb
		}},
		{"wrong key", func(_ *testing.T, flow *sso.Flow, _ *http.Cookie, cb *http.Request) (*sso.Flow, *http.Request) {
			other := *flow
			other.Key = sso.StateKey([]byte("a different server key of 32 by."))
			return &other, cb
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := ssotest.New(t)
			fake.SetProfile(testProfile)
			flow := newFlow(t, fake)
			cookie, callback := start(t, flow, sso.ModeSignIn, "", "")
			finishFlow, request := tc.mutate(t, flow, cookie, callback)

			_, _, err := finishFlow.Finish(httptest.NewRecorder(), request, "google")

			assert.ErrorIs(t, err, sso.ErrBadState)
		})
	}
}

func TestFlow_should_reject_state_made_for_a_different_provider(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetProfile(testProfile)
	flow := newFlow(t, fake)
	other := flow.Providers["google"]
	other.Name = "github"
	flow.Providers["github"] = other
	_, callback := start(t, flow, sso.ModeSignIn, "", "")

	_, _, err := flow.Finish(httptest.NewRecorder(), callback, "github")

	assert.ErrorIs(t, err, sso.ErrBadState)
}

func TestFlow_should_reject_unknown_provider(t *testing.T) {
	flow := newFlow(t, ssotest.New(t))

	err := flow.Start(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/start", nil), "github", sso.ModeSignIn, "", "")

	assert.ErrorIs(t, err, sso.ErrUnknownProvider)
}

func TestFlow_should_report_denied_with_pending_when_provider_sends_error(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetDeny(true)
	flow := newFlow(t, fake)
	_, callback := start(t, flow, sso.ModeInvite, "", "invite-code")

	pending, _, err := flow.Finish(httptest.NewRecorder(), callback, "google")

	require.ErrorIs(t, err, sso.ErrDenied)
	assert.Equal(t, "invite-code", pending.Code)
}

func TestFlow_should_fail_when_code_was_issued_for_a_different_verifier(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetProfile(testProfile)
	flow := newFlow(t, fake)
	_, victim := start(t, flow, sso.ModeSignIn, "", "")
	attackerCookie, attacker := start(t, flow, sso.ModeSignIn, "", "")
	// The attacker replays the victim's code inside their own flow.
	injected := withQuery(t, attacker, "code", victim.URL.Query().Get("code"))
	require.Equal(t, attackerCookie.Value, injected.Cookies()[0].Value)

	_, _, err := flow.Finish(httptest.NewRecorder(), injected, "google")

	require.Error(t, err)
	assert.NotErrorIs(t, err, sso.ErrBadState)
}

func TestFlow_should_reject_profile_body_over_limit(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetProfile(`{"sub":"g-123","name":"` + strings.Repeat("a", 1048576) + `"}`)
	flow := newFlow(t, fake)
	_, callback := start(t, flow, sso.ModeSignIn, "", "")

	_, _, err := flow.Finish(httptest.NewRecorder(), callback, "google")

	assert.ErrorContains(t, err, "body too large")
}

func TestFlow_should_reject_profile_without_subject(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetProfile(`{"email":"p@example.com","email_verified":true}`)
	flow := newFlow(t, fake)
	_, callback := start(t, flow, sso.ModeSignIn, "", "")

	_, _, err := flow.Finish(httptest.NewRecorder(), callback, "google")

	assert.ErrorContains(t, err, "no subject")
}

func TestGoogle_should_drop_unverified_email(t *testing.T) {
	fake := ssotest.New(t)
	fake.SetProfile(`{"sub":"g-123","email":"p@example.com","email_verified":false}`)
	flow := newFlow(t, fake)
	_, callback := start(t, flow, sso.ModeSignIn, "", "")

	_, id, err := flow.Finish(httptest.NewRecorder(), callback, "google")

	require.NoError(t, err)
	assert.Equal(t, sso.Identity{Provider: "google", Subject: "g-123"}, id)
}

func TestProviderOrigin_should_return_scheme_and_host_of_authorize_endpoint(t *testing.T) {
	google := sso.Google("id", "secret", testCallback)

	assert.Equal(t, "https://accounts.google.com", google.Origin())
}

func TestFlow_nil_has_no_providers(t *testing.T) {
	var flow *sso.Flow

	assert.Empty(t, flow.Enabled())
	assert.Empty(t, flow.Origins())
}
