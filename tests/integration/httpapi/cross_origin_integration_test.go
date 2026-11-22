//go:build integration

// e2e:bucket a

package httpapi_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfOrigin stands for the test server URL, which is known only after start.
const selfOrigin = "self"

// crossOriginCases are the browser headers of a form post. Every case sends a
// valid CSRF value, so only the origin decides.
var crossOriginCases = []struct {
	name      string
	origin    string
	fetchSite string
	wantPass  bool
}{
	{"refused_for_another_origin", "https://evil.example", "", false},
	{"refused_for_a_cross_site_fetch", "", "cross-site", false},
	{"works_for_the_same_origin", selfOrigin, "", true},
	{"works_for_a_same_origin_fetch", "", "same-origin", true},
	{"works_with_no_origin", "", "", true},
}

func postFormWithOrigin(t *testing.T, client *http.Client, target string, form url.Values, origin, fetchSite string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if fetchSite != "" {
		req.Header.Set("Sec-Fetch-Site", fetchSite)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

// wantOriginResult is the logout status and whether the session is still live.
func wantOriginResult(pass bool) []any {
	if pass {
		return []any{http.StatusSeeOther, false}
	}
	return []any{http.StatusForbidden, true}
}

func TestControlPanel_logout_should_check_the_request_origin(t *testing.T) {
	for _, tc := range crossOriginCases {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			srv, _ := newControlPanelAndPlayerServer(t, c)
			origin := tc.origin
			if origin == selfOrigin {
				origin = srv.URL
			}
			seedControlPanelUser(t, c, "owner@example.com", "correct-horse-battery-staple", false)
			cookie, csrf := controlPanelLoginCookieAndCSRF(t, srv.URL, "owner@example.com", "correct-horse-battery-staple")

			resp := postFormWithOrigin(t, noRedirectClient(), srv.URL+"/v1/control-panel/logout", url.Values{"_csrf": {csrf}}, origin, tc.fetchSite, cookie)
			resp.Body.Close()
			req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/control-panel", nil)
			require.NoError(t, err)
			req.AddCookie(cookie)
			check, err := noRedirectClient().Do(req)
			require.NoError(t, err)
			check.Body.Close()

			assert.Equal(t, wantOriginResult(tc.wantPass), []any{resp.StatusCode, check.StatusCode == http.StatusOK})
		})
	}
}

func TestPlayerAccount_logout_should_check_the_request_origin(t *testing.T) {
	for _, tc := range crossOriginCases {
		t.Run(tc.name, func(t *testing.T) {
			c := startCluster(t)
			srv, _ := newControlPanelAndPlayerServer(t, c)
			origin := tc.origin
			if origin == selfOrigin {
				origin = srv.URL
			}
			seedVerifiedPlayerAccount(t, c, "player@example.com", "correct-horse-battery-staple")
			base := srv.URL + "/v1/players/account"
			client := loginPlayerAccount(t, base, "player@example.com", "correct-horse-battery-staple")
			csrf := getPlayerCSRF(t, client, base+"/")

			resp := postFormWithOrigin(t, client, base+"/logout", url.Values{"_csrf": {csrf}}, origin, tc.fetchSite, nil)
			resp.Body.Close()
			check, err := client.Get(base + "/")
			require.NoError(t, err)
			check.Body.Close()

			assert.Equal(t, wantOriginResult(tc.wantPass), []any{resp.StatusCode, check.StatusCode == http.StatusOK})
		})
	}
}

// Games call the bearer API from other origins with CORS, so the cross-origin
// check must not apply to it: a cross-site post gets the same answer as a
// post with no Origin.
func TestBearerAPI_should_not_refuse_a_cross_site_post(t *testing.T) {
	c := startCluster(t)
	srv, _ := newControlPanelAndPlayerServer(t, c)
	login := srv.URL + "/v1/auth/login"
	plain := postFormWithOrigin(t, noRedirectClient(), login, url.Values{}, "", "", nil)
	plainBody, err := io.ReadAll(plain.Body)
	require.NoError(t, err)
	plain.Body.Close()

	resp := postFormWithOrigin(t, noRedirectClient(), login, url.Values{}, "https://evil.example", "cross-site", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	require.NotEqual(t, http.StatusForbidden, plain.StatusCode, string(plainBody))
	assert.Equal(t, []any{plain.StatusCode, string(plainBody)}, []any{resp.StatusCode, string(body)})
}
