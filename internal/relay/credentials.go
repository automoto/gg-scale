// Package relay wraps a pion/turn/v5 TURN server with ggscale's tenant +
// player identity model. Credentials are issued via the standard TURN
// REST API (RFC draft-uberti-rtcweb-turn-rest-00) so any client SDK can
// consume them; the server-side AuthHandler reconstructs the HMAC password
// for incoming TURN auth checks.
//
// The username embeds a short key id so multiple shared secrets can be
// accepted at once: the active (first) secret signs new credentials while
// older secrets stay valid, giving a zero-downtime rotation window equal to
// the credential TTL.
package relay

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SHA-1 is mandated by the TURN-REST spec
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
// (unparseable username, unknown key id, HMAC mismatch).
var ErrCredentialsInvalid = errors.New("relay: credentials invalid")

// Credentials are the values returned to a client. URLs is the operator-
// provided list of TURN URI strings the client should dial.
type Credentials struct {
	Username   string   `json:"username" example:"1767225600:3:42:k1"`
	Password   string   `json:"password" example:"dGVzdC1obWFjLXNhbXBsZS12YWx1ZQ=="`
	TTLSeconds int64    `json:"ttl" example:"600"`
	Realm      string   `json:"realm" example:"ggscale"`
	URLs       []string `json:"urls,omitempty" example:"turn:relay.example.com:3478?transport=udp"`
	STUNURLs   []string `json:"stun_urls,omitempty" example:"stun:stun.example.com:3478"`
}

// secretEntry is one accepted shared secret plus its stable key id.
type secretEntry struct {
	kid string
	key []byte
}

// Issuer mints and verifies short-lived TURN-REST credentials. One Issuer
// per process; safe for concurrent use once constructed.
type Issuer struct {
	secrets  []secretEntry // secrets[0] signs new credentials; all are accepted
	byKID    map[string]secretEntry
	realm    string
	ttl      time.Duration
	urls     []string
	stunURLs []string
	now      func() time.Time
}

// NewIssuer returns an Issuer with a single shared secret and realm. ttl is
// the validity window encoded into each issued username; the server-side
// AuthHandler rejects requests after this elapses.
func NewIssuer(secret, realm string, ttl time.Duration) *Issuer {
	return NewIssuerWithSecrets([]string{secret}, realm, ttl)
}

// NewIssuerWithSecrets returns an Issuer whose first secret signs new
// credentials and every non-empty secret is accepted for verification. This
// backs zero-downtime rotation: configure old + new together, promote the new
// secret to first, then drop the old one after a credential-TTL overlap.
func NewIssuerWithSecrets(secrets []string, realm string, ttl time.Duration) *Issuer {
	i := &Issuer{realm: realm, ttl: ttl, now: time.Now, byKID: make(map[string]secretEntry)}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		e := secretEntry{kid: kidFor(s), key: []byte(s)}
		if _, dup := i.byKID[e.kid]; dup {
			continue
		}
		i.secrets = append(i.secrets, e)
		i.byKID[e.kid] = e
	}
	return i
}

// kidFor derives a stable, non-invertible key id from a secret. 32 bits is
// ample to disambiguate the two or three secrets live during a rotation and
// reveals nothing usable about a >=32-byte secret.
func kidFor(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:4])
}

// SetURLs sets the list of TURN URIs reported in issued credentials.
func (i *Issuer) SetURLs(urls []string) { i.urls = urls }

// SetSTUNURLs sets the discovery-only STUN URIs returned alongside TURN
// credentials so WebRTC clients can gather direct server-reflexive candidates.
func (i *Issuer) SetSTUNURLs(urls []string) { i.stunURLs = urls }

// Issue returns a fresh credential pair scoped to (tenantID, playerID), signed
// by the active secret and tagged with its key id.
func (i *Issuer) Issue(tenantID, playerID int64) (*Credentials, error) {
	if len(i.secrets) == 0 {
		return nil, errors.New("relay: no signing secret configured")
	}
	active := i.secrets[0]
	expires := i.now().Add(i.ttl).Unix()
	username := fmt.Sprintf("%d:%d:%d:%s", expires, tenantID, playerID, active.kid)
	return &Credentials{
		Username:   username,
		Password:   passwordWith(active.key, username),
		TTLSeconds: int64(i.ttl.Seconds()),
		Realm:      i.realm,
		URLs:       i.urls,
		STUNURLs:   i.stunURLs,
	}, nil
}

// Verify checks the username + password pair against every accepted secret,
// returning the embedded (tenantID, playerID) on success. The in-process TURN
// server authenticates via authForUsername (pion recomputes and compares the
// HMAC key itself); Verify is the standalone entrypoint for verifying a
// credential pair out of band — e.g. an external relay or a conformance test.
func (i *Issuer) Verify(username, password string) (int64, int64, error) {
	tenantID, playerID, _, err := i.parseUsername(username)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range i.secrets {
		if subtle.ConstantTimeCompare([]byte(passwordWith(e.key, username)), []byte(password)) == 1 {
			return tenantID, playerID, nil
		}
	}
	return 0, 0, ErrCredentialsInvalid
}

// authForUsername resolves a valid TURN-REST username to its stable allocation
// subject and HMAC password. The full username remains part of the integrity
// key, while the subject deliberately excludes its expiry and signing-key id so
// renewed credentials continue to control the same live allocation.
func (i *Issuer) authForUsername(username string) (tenantID, playerID int64, password string, ok bool) {
	tenantID, playerID, kid, err := i.parseUsername(username)
	if err != nil {
		return 0, 0, "", false
	}
	e, ok := i.byKID[kid]
	if !ok {
		return 0, 0, "", false
	}
	return tenantID, playerID, passwordWith(e.key, username), true
}

func allocationUserID(tenantID, playerID int64) string {
	return strconv.FormatInt(tenantID, 10) + ":" + strconv.FormatInt(playerID, 10)
}

func parseAllocationUserID(userID string) (tenantID, playerID int64, ok bool) {
	parts := strings.Split(userID, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	tenantID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	playerID, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return tenantID, playerID, true
}

// parseUsername splits and validates the "expires:tenant:player:kid" username,
// returning ErrCredentialsExpired past the encoded expiry and
// ErrCredentialsInvalid on any structural problem.
func (i *Issuer) parseUsername(username string) (tenantID, playerID int64, kid string, err error) {
	parts := strings.SplitN(username, ":", 4)
	if len(parts) != 4 {
		return 0, 0, "", ErrCredentialsInvalid
	}
	expires, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, "", ErrCredentialsInvalid
	}
	if i.now().Unix() > expires {
		return 0, 0, "", ErrCredentialsExpired
	}
	tenantID, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, "", ErrCredentialsInvalid
	}
	playerID, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return 0, 0, "", ErrCredentialsInvalid
	}
	if parts[3] == "" {
		return 0, 0, "", ErrCredentialsInvalid
	}
	return tenantID, playerID, parts[3], nil
}

func passwordWith(secret []byte, username string) string {
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(username))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
