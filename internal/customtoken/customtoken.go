// Package customtoken holds the shared pieces of the public-key custom-token
// scheme: the developer's backend signs a JWT with its private key and
// ggscale verifies it against the tenant's stored public key, so the
// database holds no signing capability at all.
package customtoken

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
)

// MinRSABits is the smallest accepted RSA modulus. 2048 is the floor every
// current standard agrees on; smaller keys are factorable at plausible cost.
const MinRSABits = 2048

var errInvalidPublicKey = errors.New("customtoken: need a PEM-encoded Ed25519 or RSA (>= 2048 bit) public key")

// ParsePublicKey parses a PEM "PUBLIC KEY" block into a verification key.
// Ed25519 and RSA (>= MinRSABits) are supported — the two algorithms the
// custom-token verifier pins. Trailing data after the block is rejected so a
// stored value is exactly one key.
func ParsePublicKey(pemText string) (crypto.PublicKey, error) {
	block, rest := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "PUBLIC KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, errInvalidPublicKey
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errInvalidPublicKey
	}
	switch key := parsed.(type) {
	case ed25519.PublicKey:
		return key, nil
	case *rsa.PublicKey:
		if key.N.BitLen() < MinRSABits {
			return nil, errInvalidPublicKey
		}
		return key, nil
	default:
		return nil, errInvalidPublicKey
	}
}
