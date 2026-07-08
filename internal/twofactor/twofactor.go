// Package twofactor holds the TOTP two-factor primitives shared by the
// control panel and player-account login surfaces: secret encryption at rest,
// RFC 6238 code validation with replay protection, backup codes, and QR
// rendering for enrollment. Key provisioning (Load) is package-owned;
// per-user credential persistence stays with the callers because the two
// surfaces key their tables differently.
package twofactor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/ggscale/ggscale/internal/signedcookie"
)

const (
	// MaxAttempts is the number of failed challenge submissions allowed
	// before the credential locks for LockoutDuration.
	MaxAttempts = 5
	// LockoutDuration is how long the challenge stays locked after
	// MaxAttempts failures.
	LockoutDuration = 15 * time.Minute

	// TrustedDeviceTTL is how long a "remember this device" grant lasts.
	TrustedDeviceTTL = 30 * 24 * time.Hour

	// PendingTTL bounds the window between password success and the
	// challenge submission.
	PendingTTL = 10 * time.Minute

	// BackupCodeCount is how many single-use recovery codes an enrollment
	// mints.
	BackupCodeCount = 10

	totpDigits = 6
	period     = 30

	secretVersion byte = 1

	// backupAlphabet is lowercase base32: unambiguous to read back and
	// case-insensitive on input via normalization.
	backupAlphabet = "abcdefghijklmnopqrstuvwxyz234567"
	backupCodeLen  = 10
)

// Cipher encrypts TOTP secrets at rest with AES-256-GCM and derives the
// HMAC key used to sign challenge-pending cookies. The primary key seals
// new secrets; fallback keys only open older ones, so an operator can move
// from the auto-generated database key to TWO_FACTOR_ENC_KEY without
// locking out existing enrollments. A nil *Cipher means the feature is not
// configured (unit tests; real deployments always get a key from Load).
type Cipher struct {
	aead       cipher.AEAD
	fallbacks  []cipher.AEAD
	pendingKey []byte
}

// NewCipher builds a single-key Cipher from a 64-char hex string (32-byte
// key). An empty key returns (nil, nil): the feature is off, not
// misconfigured.
func NewCipher(hexKey string) (*Cipher, error) {
	if hexKey == "" {
		return nil, nil
	}
	key, err := decodeHexKey(hexKey)
	if err != nil {
		return nil, err
	}
	return NewCipherKeyring(key)
}

// NewCipherKeyring builds a Cipher from raw 32-byte keys: the first is the
// primary (encrypts, derives PendingKey), the rest are decrypt-only.
func NewCipherKeyring(keys ...[]byte) (*Cipher, error) {
	if len(keys) == 0 {
		return nil, errors.New("two-factor keyring: no keys")
	}
	aeads := make([]cipher.AEAD, 0, len(keys))
	for _, key := range keys {
		if len(key) != 32 {
			return nil, fmt.Errorf("two-factor key: need 32 bytes, got %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("two-factor key: %w", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("two-factor key: %w", err)
		}
		aeads = append(aeads, aead)
	}
	pendingKey, err := hkdf.Key(sha256.New, keys[0], nil, "ggscale-2fa-pending", 32)
	if err != nil {
		return nil, fmt.Errorf("two-factor key: %w", err)
	}
	return &Cipher{aead: aeads[0], fallbacks: aeads[1:], pendingKey: pendingKey}, nil
}

func decodeHexKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("two-factor key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("two-factor key: need 32 bytes, got %d", len(key))
	}
	return key, nil
}

// PendingKey is the derived HMAC key for challenge-pending cookies. Unlike
// a per-process random key it is stable across restarts and instances.
func (c *Cipher) PendingKey() []byte {
	return c.pendingKey
}

// Encrypt seals plaintext as version || nonce || ciphertext. The framing is
// owned here so the schema stores a single opaque column.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	out = append(out, secretVersion)
	out = append(out, nonce...)
	return c.aead.Seal(out, nonce, plaintext, nil), nil
}

// Decrypt opens data produced by Encrypt, trying the primary key first and
// then each fallback key.
func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	if len(data) < 1+c.aead.NonceSize() {
		return nil, errors.New("two-factor secret: truncated")
	}
	if data[0] != secretVersion {
		return nil, fmt.Errorf("two-factor secret: unknown version %d", data[0])
	}
	nonce := data[1 : 1+c.aead.NonceSize()]
	ciphertext := data[1+c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err == nil {
		return plaintext, nil
	}
	for _, fallback := range c.fallbacks {
		if plaintext, fbErr := fallback.Open(nil, nonce, ciphertext, nil); fbErr == nil {
			return plaintext, nil
		}
	}
	return nil, err
}

// GenerateKey creates a fresh TOTP key with RFC-default parameters (30s
// period, SHA1, 6 digits) — the compatibility baseline for authenticator
// apps.
func GenerateKey(issuer, email string) (*otp.Key, error) {
	return totp.Generate(totp.GenerateOpts{Issuer: issuer, AccountName: email})
}

// KeyFromParts rebuilds an otp.Key from a stored secret so the enrollment
// QR can be re-rendered (e.g. after a failed confirm) without minting a
// new secret. Mirrors the otpauth URL shape totp.Generate produces.
func KeyFromParts(issuer, email, secret string) (*otp.Key, error) {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	u := url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + email,
		RawQuery: v.Encode(),
	}
	return otp.NewKeyFromURL(u.String())
}

// ValidateCode checks code against secret at now, allowing one period of
// clock skew either way, and returns the matched timestep. It performs no
// replay check: the caller persists the returned step through an atomic
// "last_used_step < step" update, which is the single race-free arbiter of
// whether a code has already been spent. Folding replay detection in here
// would reintroduce a check-then-act race (a valid code rejected in Go still
// counts against the lockout), so that responsibility stays with the store.
func ValidateCode(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if !IsTOTPCode(code) {
		return 0, false
	}
	step := now.Unix() / period
	matched := int64(-1)
	for _, candidate := range []int64{step - 1, step, step + 1} {
		expected, err := totp.GenerateCodeCustom(secret, time.Unix(candidate*period, 0), totp.ValidateOpts{
			Period: period, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			matched = candidate
		}
	}
	if matched < 0 {
		return 0, false
	}
	return matched, true
}

// IsTOTPCode reports whether input looks like an authenticator code (six
// digits); anything else is routed to the backup-code path.
func IsTOTPCode(input string) bool {
	input = strings.TrimSpace(input)
	if len(input) != totpDigits {
		return false
	}
	for _, r := range input {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// GenerateBackupCodes returns BackupCodeCount single-use recovery codes in
// xxxxx-xxxxx form, each 10 chars of lowercase base32 (50 bits of entropy).
func GenerateBackupCodes() ([]string, error) {
	codes := make([]string, 0, BackupCodeCount)
	for range BackupCodeCount {
		raw := make([]byte, backupCodeLen)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		var b strings.Builder
		for i, v := range raw {
			if i == backupCodeLen/2 {
				b.WriteByte('-')
			}
			b.WriteByte(backupAlphabet[int(v)%len(backupAlphabet)])
		}
		codes = append(codes, b.String())
	}
	return codes, nil
}

// HashBackupCode hashes a normalized backup code for storage and lookup.
// Codes are crypto-random, so SHA-256 (not bcrypt) is sufficient and keeps
// the pre-session challenge endpoint cheap.
func HashBackupCode(code string) []byte {
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' {
			return -1
		}
		return unicode.ToLower(r)
	}, code)
	sum := sha256.Sum256([]byte(normalized))
	return sum[:]
}

// QRPNGDataURI renders the enrollment QR as a PNG data URI, displayable
// under the CSP img-src 'self' data: policy without any JavaScript.
func QRPNGDataURI(key *otp.Key) (string, error) {
	img, err := key.Image(220, 220)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("data:image/png;base64,")
	enc := base64.NewEncoder(base64.StdEncoding, &b)
	if err := png.Encode(enc, img); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Pending is the signed state parked in a cookie between password success
// and the challenge submission. Subject is the user's ID in string form so
// one codec serves both surfaces (int64 control panel IDs, UUID player IDs).
type Pending struct {
	Purpose   string `json:"purpose"`
	Subject   string `json:"subject"`
	Email     string `json:"email"`
	ExpiresAt int64  `json:"expires_at"`
}

// pendingPurpose tags 2FA pending payloads so a payload signed for another
// flow can never open as a challenge grant, even under the same key.
const pendingPurpose = "2fa"

// EncodePending signs p for the 2FA pending cookie. The purpose tag is
// forced here so callers cannot mint a payload for a different flow.
func EncodePending(key []byte, p Pending) string {
	p.Purpose = pendingPurpose
	return encodePendingRaw(key, p)
}

func encodePendingRaw(key []byte, p Pending) string {
	payload, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return signedcookie.Sign(key, payload)
}

// DecodePending verifies the signature, purpose tag, and server-side expiry.
// The cookie's own MaxAge is client-controlled and not trusted.
func DecodePending(key []byte, raw string, now time.Time) (Pending, bool) {
	payload, ok := signedcookie.Open(key, raw)
	if !ok {
		return Pending{}, false
	}
	var p Pending
	if err := json.Unmarshal(payload, &p); err != nil {
		return Pending{}, false
	}
	if p.Purpose != pendingPurpose || now.Unix() > p.ExpiresAt {
		return Pending{}, false
	}
	return p, true
}

// GroupSecret formats a base32 secret in 4-char groups for manual entry.
func GroupSecret(secret string) string {
	var b strings.Builder
	for i, r := range secret {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
