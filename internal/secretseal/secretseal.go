// Package secretseal encrypts stored provider credentials that must be
// replayed to a third party (the Steam Web API key today, the itch key
// later) with AES-256-GCM. It mirrors the two-factor cipher: versioned
// framing, a primary key that seals new values, and decrypt-only fallback
// keys so key rotation never locks stored credentials out. Credentials that
// only verify signatures (custom tokens) store public keys instead and need
// no sealing.
package secretseal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

const sealVersion byte = 1

// Cipher seals credential values at rest. A nil *Cipher is valid for read
// helpers (DecryptOrPlain) and means sealing is not configured.
type Cipher struct {
	aead      cipher.AEAD
	fallbacks []cipher.AEAD
}

// NewCipherKeyring builds a Cipher from raw 32-byte keys: the first is the
// primary (encrypts new values), the rest are decrypt-only fallbacks.
func NewCipherKeyring(keys ...[]byte) (*Cipher, error) {
	if len(keys) == 0 {
		return nil, errors.New("secretseal keyring: no keys")
	}
	aeads := make([]cipher.AEAD, 0, len(keys))
	for _, key := range keys {
		if len(key) != 32 {
			return nil, fmt.Errorf("secretseal key: need 32 bytes, got %d", len(key))
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("secretseal key: %w", err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("secretseal key: %w", err)
		}
		aeads = append(aeads, aead)
	}
	return &Cipher{aead: aeads[0], fallbacks: aeads[1:]}, nil
}

func decodeHexKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("secretseal key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secretseal key: need 32 bytes, got %d", len(key))
	}
	return key, nil
}

// Encrypt seals plaintext as version || nonce || ciphertext, so the schema
// stores a single opaque column.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+c.aead.Overhead())
	out = append(out, sealVersion)
	out = append(out, nonce...)
	return c.aead.Seal(out, nonce, plaintext, nil), nil
}

// Decrypt opens data produced by Encrypt, trying the primary key first and
// then each fallback key.
func (c *Cipher) Decrypt(data []byte) ([]byte, error) {
	if len(data) < 1+c.aead.NonceSize() {
		return nil, errors.New("secretseal: truncated")
	}
	if data[0] != sealVersion {
		return nil, fmt.Errorf("secretseal: unknown version %d", data[0])
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

// DecryptOrPlain returns the decrypted value when data is a sealed frame and
// the input unchanged otherwise. It keeps legacy plaintext rows (values set
// before sealing existed, or seeded directly via SQL) working, and is safe on
// a nil receiver. The GCM auth tag makes a false positive on plaintext
// practically impossible.
func (c *Cipher) DecryptOrPlain(data []byte) []byte {
	if c == nil {
		return data
	}
	if plaintext, err := c.Decrypt(data); err == nil {
		return plaintext
	}
	return data
}
