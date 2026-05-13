// Package relay wraps a pion/turn/v3 TURN server with ggscale's tenant +
// end-user identity model. Credentials are issued via the standard TURN
// REST API (RFC draft-uberti-rtcweb-turn-rest-00) so any client SDK can
// consume them; the server-side AuthHandler reconstructs the HMAC password
// for incoming TURN auth checks.
package relay

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is mandated by the TURN-REST spec
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrCredentialsExpired is returned by Verify when the encoded expiry has
// already elapsed.
var ErrCredentialsExpired = errors.New("relay: credentials expired")

// ErrCredentialsInvalid is returned by Verify on any other failure
// (unparseable username, HMAC mismatch).
var ErrCredentialsInvalid = errors.New("relay: credentials invalid")

// Credentials are the values returned to a client. URLs is the operator-
// provided list of TURN URI strings the client should dial.
type Credentials struct {
	Username   string   `json:"username"`
	Password   string   `json:"password"`
	TTLSeconds int64    `json:"ttl"`
	Realm      string   `json:"realm"`
	URLs       []string `json:"urls,omitempty"`
}

// Issuer mints and verifies short-lived TURN-REST credentials. One Issuer
// per process; safe for concurrent use.
type Issuer struct {
	secret []byte
	realm  string
	ttl    time.Duration
	urls   []string
	now    func() time.Time
}

// NewIssuer returns an Issuer with the given shared secret and realm. ttl
// is the validity window encoded into each issued username; the server-
// side AuthHandler rejects requests after this elapses.
func NewIssuer(secret, realm string, ttl time.Duration) *Issuer {
	return &Issuer{secret: []byte(secret), realm: realm, ttl: ttl, now: time.Now}
}

// SetURLs sets the list of TURN URIs reported in issued credentials.
func (i *Issuer) SetURLs(urls []string) { i.urls = urls }

// Issue returns a fresh credential pair scoped to (tenantID, endUserID).
func (i *Issuer) Issue(tenantID, endUserID int64) (*Credentials, error) {
	expires := i.now().Add(i.ttl).Unix()
	username := fmt.Sprintf("%d:%d:%d", expires, tenantID, endUserID)
	password := i.passwordFor(username)
	return &Credentials{
		Username:   username,
		Password:   password,
		TTLSeconds: int64(i.ttl.Seconds()),
		Realm:      i.realm,
		URLs:       i.urls,
	}, nil
}

// Verify checks the username + password pair, returning the embedded
// (tenantID, endUserID) on success.
func (i *Issuer) Verify(username, password string) (int64, int64, error) {
	parts := strings.SplitN(username, ":", 3)
	if len(parts) != 3 {
		return 0, 0, ErrCredentialsInvalid
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, ErrCredentialsInvalid
	}
	if i.now().Unix() > expires {
		return 0, 0, ErrCredentialsExpired
	}
	tenantID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, ErrCredentialsInvalid
	}
	endUserID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, 0, ErrCredentialsInvalid
	}
	want := i.passwordFor(username)
	if subtle.ConstantTimeCompare([]byte(want), []byte(password)) != 1 {
		return 0, 0, ErrCredentialsInvalid
	}
	return tenantID, endUserID, nil
}

func (i *Issuer) passwordFor(username string) string {
	mac := hmac.New(sha1.New, i.secret)
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
