package customtoken

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pemPublicKey(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestParsePublicKey(t *testing.T) {
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	rsa2048, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rsa1024, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"ed25519", pemPublicKey(t, edPub), false},
		{"rsa_2048", pemPublicKey(t, &rsa2048.PublicKey), false},
		{"rsa_1024_too_small", pemPublicKey(t, &rsa1024.PublicKey), true},
		{"ecdsa_unsupported", pemPublicKey(t, &ecKey.PublicKey), true},
		{"empty", "", true},
		{"garbage", "not a pem block", true},
		{"trailing_data", pemPublicKey(t, edPub) + "extra", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := ParsePublicKey(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, key)
		})
	}
}

func TestParsePublicKey_rejects_private_key_pem(t *testing.T) {
	_, edPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(edPriv)
	require.NoError(t, err)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	_, err = ParsePublicKey(pemText)

	assert.Error(t, err, "a pasted private key must be rejected, not stored")
}
