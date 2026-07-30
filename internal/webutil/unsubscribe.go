package webutil

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// unsubscribeMAC domain-separates the HMAC from the other uses of the shared
// email signing key.
const unsubscribeMAC = "email-unsubscribe:"

// EmailUnsubscribeToken mints the signed token carried by one-click
// unsubscribe links: base64url(email) + "." + base64url(hmac). The token
// authorizes exactly one action — suppressing that address — so it needs no
// expiry and no login.
func EmailUnsubscribeToken(key []byte, email string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsubscribeMAC + email))
	return base64.RawURLEncoding.EncodeToString([]byte(email)) +
		"." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// ParseEmailUnsubscribeToken verifies a token and returns the address it
// authorizes suppressing. ok is false for malformed or forged tokens.
func ParseEmailUnsubscribeToken(key []byte, token string) (string, bool) {
	encEmail, encMAC, found := strings.Cut(token, ".")
	if !found {
		return "", false
	}
	emailBytes, err := base64.RawURLEncoding.DecodeString(encEmail)
	if err != nil {
		return "", false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(encMAC)
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsubscribeMAC + string(emailBytes)))
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return "", false
	}
	return string(emailBytes), true
}

// EmailUnsubscribeURL builds the absolute one-click unsubscribe link for the
// given address. baseURL empty renders a relative path (dev).
func EmailUnsubscribeURL(baseURL string, key []byte, email string) string {
	return strings.TrimRight(baseURL, "/") + "/v1/unsubscribe?token=" + EmailUnsubscribeToken(key, email)
}
