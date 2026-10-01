// Package ssotest is a fake OAuth 2.0 provider for tests. It implements the
// authorize, token, and profile endpoints and enforces PKCE, so a test
// exercises the same round trip a real provider does.
package ssotest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

// Fake is one fake provider. Set Profile to the JSON document the profile
// endpoint returns, and Deny to make the authorize endpoint answer with an
// error in place of a code.
type Fake struct {
	Server *httptest.Server

	mu         sync.Mutex
	profile    string
	deny       bool
	challenges map[string]string // code -> PKCE challenge
	next       int
}

// New starts a fake provider that is closed when the test ends.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{challenges: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", f.authorize)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/profile", f.serveProfile)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// Endpoint is the oauth2 endpoint of the fake.
func (f *Fake) Endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{
		AuthURL:   f.Server.URL + "/authorize",
		TokenURL:  f.Server.URL + "/token",
		AuthStyle: oauth2.AuthStyleInParams,
	}
}

// ProfileURL is the profile endpoint of the fake.
func (f *Fake) ProfileURL() string { return f.Server.URL + "/profile" }

// SetProfile sets the JSON document the profile endpoint returns.
func (f *Fake) SetProfile(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profile = body
}

// SetDeny makes the authorize endpoint refuse, as when consent is cancelled.
func (f *Fake) SetDeny(deny bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deny = deny
}

func (f *Fake) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	out := url.Values{"state": {q.Get("state")}}
	f.mu.Lock()
	if f.deny {
		out.Set("error", "access_denied")
	} else {
		f.next++
		code := "code-" + strconv.Itoa(f.next)
		f.challenges[code] = q.Get("code_challenge")
		out.Set("code", code)
	}
	f.mu.Unlock()
	redirect.RawQuery = out.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	code := r.Form.Get("code")
	f.mu.Lock()
	challenge, ok := f.challenges[code]
	delete(f.challenges, code) // a code works one time
	f.mu.Unlock()
	if !ok || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "token-for-" + code,
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

func (f *Fake) serveProfile(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	body := f.profile
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}
