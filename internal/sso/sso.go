// Package sso holds the provider protocol mechanics for single sign-on: the
// OAuth 2.0 code flow with PKCE and the signed state cookie that binds a
// browser to one attempt. It has no database code and no knowledge of players
// or control panel users; the two surfaces decide what a verified Identity
// means for their own account store.
package sso

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/oauth2"

	"github.com/automoto/gg-scale/internal/signedcookie"
)

// Modes a flow can run in. The mode is fixed at start and signed into the
// state cookie, so a callback cannot be steered into a different mode.
const (
	ModeSignIn       = "signin"
	ModeLink         = "link"
	ModeInvite       = "invite"
	ModeSignupAccept = "signup_accept"
)

const (
	stateTTL        = 10 * time.Minute
	providerTimeout = 5 * time.Second
	maxProfileBytes = 1048576
)

var (
	// ErrUnknownProvider means the provider is not enabled on this surface.
	ErrUnknownProvider = errors.New("sso: unknown provider")
	// ErrBadState means the state cookie is absent, forged, expired, made
	// for a different surface or provider, or does not match the callback.
	ErrBadState = errors.New("sso: bad state")
	// ErrDenied means the provider returned an error in place of a code,
	// for example because the person cancelled the consent screen.
	ErrDenied = errors.New("sso: provider returned an error")
)

// Identity is the verified result of one provider round trip.
type Identity struct {
	Provider      string
	Subject       string // immutable provider ID
	Email         string // set only when EmailVerified is true
	EmailVerified bool
	Name          string // optional, used to seed a display name
}

// Provider is an OAuth 2.0 code-flow provider. Providers differ only in
// data, and the endpoint and profile URL are fields so a test can point
// them at a local server.
type Provider struct {
	Name       string // path segment and stored provider value
	Label      string // shown on buttons
	OAuth      oauth2.Config
	ProfileURL string
	Parse      func(body []byte) (Identity, error)
}

// Origin is the scheme and host of the authorize endpoint. A form POST that
// redirects there needs this origin in the CSP form-action directive.
func (p Provider) Origin() string {
	u, err := url.Parse(p.OAuth.Endpoint.AuthURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// exchange trades the authorization code for an access token over the back
// channel and reads the identity from the profile endpoint with it. The
// token never passes through the browser, so it cannot be substituted.
func (p Provider) exchange(ctx context.Context, code, verifier string) (Identity, error) {
	tokenCtx, cancelToken := context.WithTimeout(ctx, providerTimeout)
	defer cancelToken()
	token, err := p.OAuth.Exchange(tokenCtx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("sso: %s code exchange: %w", p.Name, err)
	}

	profileCtx, cancelProfile := context.WithTimeout(ctx, providerTimeout)
	defer cancelProfile()
	req, err := http.NewRequestWithContext(profileCtx, http.MethodGet, p.ProfileURL, nil)
	if err != nil {
		return Identity{}, fmt.Errorf("sso: %s profile request: %w", p.Name, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.OAuth.Client(profileCtx, token).Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("sso: %s profile: %w", p.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("sso: %s profile: status %d", p.Name, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileBytes+1))
	if err != nil {
		return Identity{}, fmt.Errorf("sso: %s profile read: %w", p.Name, err)
	}
	if len(body) > maxProfileBytes {
		return Identity{}, fmt.Errorf("sso: %s profile: body too large", p.Name)
	}
	id, err := p.Parse(body)
	if err != nil {
		return Identity{}, fmt.Errorf("sso: %s profile parse: %w", p.Name, err)
	}
	if id.Subject == "" {
		return Identity{}, fmt.Errorf("sso: %s profile: no subject", p.Name)
	}
	id.Provider = p.Name
	return id, nil
}

// Pending is the signed state parked in a cookie between the start of a flow
// and the provider callback.
type Pending struct {
	Purpose  string `json:"purpose"`
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Provider string `json:"provider"`
	Mode     string `json:"mode"`
	// Actor is the account that started a link, in string form so one codec
	// serves both surfaces. The callback session must be the same account.
	Actor string `json:"actor,omitempty"`
	// Code is the emailed invite or signup-accept code.
	Code      string `json:"code,omitempty"`
	ExpiresAt int64  `json:"exp"`
}

// StateKey derives the state-cookie signing key from a server key that also
// signs other cookies, so a value signed for one flow never verifies in the
// other.
func StateKey(master []byte) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("ggscale sso state"))
	return mac.Sum(nil)
}

// Flow is the state handling one surface shares across its providers.
type Flow struct {
	Providers map[string]Provider
	// Key signs the state cookie. Use StateKey to derive it.
	Key []byte
	// Purpose is a fixed string for the surface. A cookie minted by the
	// player site never opens on the control panel, and the reverse.
	Purpose    string
	CookieName string
	// CookiePath must be a prefix of the callback path.
	CookiePath string
	Secure     bool
	Now        func() time.Time
}

// Enabled returns the providers in a stable order for rendering buttons.
func (f *Flow) Enabled() []Provider {
	if f == nil {
		return nil
	}
	out := make([]Provider, 0, len(f.Providers))
	for _, name := range slices.Sorted(maps.Keys(f.Providers)) {
		out = append(out, f.Providers[name])
	}
	return out
}

// Has reports whether the provider is enabled on this surface.
func (f *Flow) Has(name string) bool {
	_, ok := f.provider(name)
	return ok
}

// Label returns the display name of an enabled provider.
func (f *Flow) Label(name string) string {
	p, _ := f.provider(name)
	return p.Label
}

// Origins returns the authorize origins of the enabled providers.
func (f *Flow) Origins() []string {
	var out []string
	for _, p := range f.Enabled() {
		if o := p.Origin(); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// Start parks a fresh state and PKCE verifier in the cookie and sends the
// browser to the provider.
func (f *Flow) Start(w http.ResponseWriter, r *http.Request, provider, mode, actor, code string) error {
	p, ok := f.provider(provider)
	if !ok {
		return ErrUnknownProvider
	}
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		return fmt.Errorf("sso: state rand: %w", err)
	}
	pending := Pending{
		Purpose:   f.Purpose,
		State:     base64.RawURLEncoding.EncodeToString(stateBytes),
		Verifier:  oauth2.GenerateVerifier(),
		Provider:  p.Name,
		Mode:      mode,
		Actor:     actor,
		Code:      code,
		ExpiresAt: f.Now().Add(stateTTL).Unix(),
	}
	payload, err := json.Marshal(pending)
	if err != nil {
		return fmt.Errorf("sso: state marshal: %w", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     f.CookieName,
		Value:    signedcookie.Sign(f.Key, payload),
		Path:     f.CookiePath,
		MaxAge:   int(stateTTL.Seconds()),
		HttpOnly: true,
		// Lax, not Strict: the provider sends the browser back with a
		// cross-site top-level GET, which must carry this cookie.
		SameSite: http.SameSiteLaxMode,
		Secure:   f.Secure,
	})
	authURL := p.OAuth.AuthCodeURL(pending.State, oauth2.S256ChallengeOption(pending.Verifier))
	http.Redirect(w, r, authURL, http.StatusSeeOther)
	return nil
}

// Finish handles the provider callback. It always deletes the state cookie
// first, so a state is single-use even when a later check fails. On
// ErrDenied and on exchange errors the returned Pending is valid, so the
// caller can send the person back to the page the flow started from.
func (f *Flow) Finish(w http.ResponseWriter, r *http.Request, provider string) (Pending, Identity, error) {
	p, ok := f.provider(provider)
	if !ok {
		return Pending{}, Identity{}, ErrUnknownProvider
	}
	cookie, cookieErr := r.Cookie(f.CookieName)
	http.SetCookie(w, &http.Cookie{
		Name:     f.CookieName,
		Value:    "",
		Path:     f.CookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   f.Secure,
	})
	if cookieErr != nil {
		return Pending{}, Identity{}, ErrBadState
	}
	pending, ok := f.decode(cookie.Value)
	if !ok || pending.Provider != p.Name {
		return Pending{}, Identity{}, ErrBadState
	}
	query := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(pending.State)) != 1 {
		return Pending{}, Identity{}, ErrBadState
	}
	if query.Get("error") != "" {
		return pending, Identity{}, ErrDenied
	}
	id, err := p.exchange(r.Context(), query.Get("code"), pending.Verifier)
	if err != nil {
		return pending, Identity{}, err
	}
	return pending, id, nil
}

func (f *Flow) provider(name string) (Provider, bool) {
	if f == nil {
		return Provider{}, false
	}
	p, ok := f.Providers[name]
	return p, ok
}

// decode verifies the signature, the purpose tag, and the server-side
// expiry. The cookie's own MaxAge is client-controlled and not trusted.
func (f *Flow) decode(raw string) (Pending, bool) {
	payload, ok := signedcookie.Open(f.Key, raw)
	if !ok {
		return Pending{}, false
	}
	var p Pending
	if err := json.Unmarshal(payload, &p); err != nil {
		return Pending{}, false
	}
	if p.Purpose != f.Purpose || p.State == "" || f.Now().Unix() > p.ExpiresAt {
		return Pending{}, false
	}
	return p, true
}
