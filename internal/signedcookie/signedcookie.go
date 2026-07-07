// Package signedcookie is the MAC-signed cookie framing shared by the flows
// that park short-lived state in a cookie: email verification and the 2FA
// pending challenge. It signs opaque bytes and leaves payload serialization to
// each caller, so there is one implementation of the sign/verify path instead
// of one per flow. Framing:
//
//	base64url(payload) + "." + base64url(HMAC-SHA256(key, payload))
package signedcookie

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
)

// Sign returns the signed cookie value for payload under key.
func Sign(key, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Open verifies raw's signature under key and returns the payload bytes. The
// bool is false on malformed input or any signature mismatch; the comparison
// is constant-time.
func Open(key []byte, raw string) ([]byte, bool) {
	encPayload, encSig, ok := strings.Cut(raw, ".")
	if !ok {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if subtle.ConstantTimeCompare(mac.Sum(nil), sig) != 1 {
		return nil, false
	}
	return payload, true
}
