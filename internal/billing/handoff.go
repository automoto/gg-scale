// Package billing holds the OSS side of the external-billing handoff: a
// signed, short-lived token the dashboard mints so the (separate, closed)
// billing service knows which authenticated tenant is upgrading. The token
// rides Stripe's client_reference_id passthrough, which Stripe does not sign
// — hence the HMAC. The billing service re-implements the ~15-line verify
// against the shared BILLING_HANDOFF_KEY; no ggscale internals cross repos.
package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultHandoffTTL bounds how long a minted upgrade link stays valid. Long
// enough to pick a plan, short enough that a leaked link goes stale.
const DefaultHandoffTTL = 15 * time.Minute

var errInvalidHandoff = errors.New("billing: invalid handoff token")

// SignHandoff mints a URL-safe token binding tenantID to an expiry:
// base64url("tenantID.exp") + "." + base64url(HMAC-SHA256(key, payload)).
func SignHandoff(key []byte, tenantID int64, ttl time.Duration, now time.Time) string {
	payload := strconv.FormatInt(tenantID, 10) + "." + strconv.FormatInt(now.Add(ttl).Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyHandoff checks the signature and expiry and returns the tenant ID.
func VerifyHandoff(key []byte, token string, now time.Time) (int64, error) {
	encPayload, encSig, ok := strings.Cut(token, ".")
	if !ok {
		return 0, errInvalidHandoff
	}
	payload, err := base64.RawURLEncoding.DecodeString(encPayload)
	if err != nil {
		return 0, errInvalidHandoff
	}
	sig, err := base64.RawURLEncoding.DecodeString(encSig)
	if err != nil {
		return 0, errInvalidHandoff
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return 0, errInvalidHandoff
	}

	tenantPart, expPart, ok := strings.Cut(string(payload), ".")
	if !ok {
		return 0, errInvalidHandoff
	}
	tenantID, err := strconv.ParseInt(tenantPart, 10, 64)
	if err != nil || tenantID <= 0 {
		return 0, errInvalidHandoff
	}
	exp, err := strconv.ParseInt(expPart, 10, 64)
	if err != nil {
		return 0, errInvalidHandoff
	}
	if now.Unix() > exp {
		return 0, fmt.Errorf("billing: handoff token expired")
	}
	return tenantID, nil
}
