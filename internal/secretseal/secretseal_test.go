package secretseal

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func TestCipher_round_trip(t *testing.T) {
	c, err := NewCipherKeyring(testKey(t))
	require.NoError(t, err)

	sealed, err := c.Encrypt([]byte("publisher-key"))
	require.NoError(t, err)
	assert.NotEqual(t, []byte("publisher-key"), sealed)

	got, err := c.Decrypt(sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("publisher-key"), got)
}

func TestCipher_wrong_key_fails(t *testing.T) {
	a, err := NewCipherKeyring(testKey(t))
	require.NoError(t, err)
	b, err := NewCipherKeyring(testKey(t))
	require.NoError(t, err)

	sealed, err := a.Encrypt([]byte("secret"))
	require.NoError(t, err)

	_, err = b.Decrypt(sealed)
	assert.Error(t, err)
}

func TestCipher_fallback_key_decrypts_older_values(t *testing.T) {
	oldKey, newKey := testKey(t), testKey(t)
	old, err := NewCipherKeyring(oldKey)
	require.NoError(t, err)
	sealed, err := old.Encrypt([]byte("legacy"))
	require.NoError(t, err)

	rotated, err := NewCipherKeyring(newKey, oldKey)
	require.NoError(t, err)
	got, err := rotated.Decrypt(sealed)
	require.NoError(t, err)
	assert.Equal(t, []byte("legacy"), got)
}

func TestCipher_rejects_short_keys(t *testing.T) {
	_, err := NewCipherKeyring([]byte("short"))
	assert.Error(t, err)
}

func TestDecryptOrPlain(t *testing.T) {
	c, err := NewCipherKeyring(testKey(t))
	require.NoError(t, err)
	sealed, err := c.Encrypt([]byte("hunter2"))
	require.NoError(t, err)

	tests := []struct {
		name   string
		cipher *Cipher
		in     []byte
		want   []byte
	}{
		{"sealed_value_decrypts", c, sealed, []byte("hunter2")},
		{"plaintext_passes_through", c, []byte("legacy-plain"), []byte("legacy-plain")},
		{"empty_passes_through", c, []byte{}, []byte{}},
		{"nil_cipher_passes_through", nil, sealed, sealed},
		{"garbage_frame_passes_through", c, []byte{1, 2, 3}, []byte{1, 2, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.cipher.DecryptOrPlain(tt.in))
		})
	}
}
