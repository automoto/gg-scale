package twofactor

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHexKey = "6368616e676520746869732070617373776f726420746f206120736563726574"

func TestNewCipher(t *testing.T) {
	tests := []struct {
		name    string
		hexKey  string
		wantNil bool
		wantErr bool
	}{
		{name: "should_return_nil_when_key_empty", hexKey: "", wantNil: true},
		{name: "should_error_on_invalid_hex", hexKey: "not-hex", wantErr: true},
		{name: "should_error_on_short_key", hexKey: "abcd1234", wantErr: true},
		{name: "should_build_cipher_from_32_byte_hex", hexKey: testHexKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewCipher(tt.hexKey)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantNil, c == nil)
		})
	}
}

func TestCipherRoundTrip(t *testing.T) {
	c, err := NewCipher(testHexKey)
	require.NoError(t, err)
	plaintext := []byte("JBSWY3DPEHPK3PXP")

	first, err := c.Encrypt(plaintext)
	require.NoError(t, err)
	second, err := c.Encrypt(plaintext)
	require.NoError(t, err)
	decrypted, err := c.Decrypt(first)
	require.NoError(t, err)

	assert.Equal(t, plaintext, decrypted)
	assert.NotEqual(t, first, second, "nonces must randomize ciphertext")
}

func TestCipherDecryptRejectsBadInput(t *testing.T) {
	c, err := NewCipher(testHexKey)
	require.NoError(t, err)
	valid, err := c.Encrypt([]byte("secret"))
	require.NoError(t, err)

	tampered := append([]byte(nil), valid...)
	tampered[len(tampered)-1] ^= 0x01
	wrongVersion := append([]byte(nil), valid...)
	wrongVersion[0] = 0xff

	tests := []struct {
		name string
		data []byte
	}{
		{name: "should_reject_tampered_ciphertext", data: tampered},
		{name: "should_reject_unknown_version", data: wrongVersion},
		{name: "should_reject_truncated_input", data: valid[:8]},
		{name: "should_reject_empty_input", data: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Decrypt(tt.data)

			assert.Error(t, err)
		})
	}
}

func TestNewCipherKeyring(t *testing.T) {
	k1 := []byte(strings.Repeat("a", 32))
	k2 := []byte(strings.Repeat("b", 32))

	tests := []struct {
		name    string
		keys    [][]byte
		wantErr bool
	}{
		{name: "should_error_with_no_keys", keys: nil, wantErr: true},
		{name: "should_error_on_short_key", keys: [][]byte{[]byte("short")}, wantErr: true},
		{name: "should_error_on_short_fallback_key", keys: [][]byte{k1, []byte("short")}, wantErr: true},
		{name: "should_build_from_one_key", keys: [][]byte{k1}},
		{name: "should_build_from_primary_and_fallback", keys: [][]byte{k1, k2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewCipherKeyring(tt.keys...)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, c)
		})
	}
}

func TestKeyringDecryptsFallbackCiphertext(t *testing.T) {
	oldKey := []byte(strings.Repeat("a", 32))
	newKey := []byte(strings.Repeat("b", 32))
	oldCipher, err := NewCipherKeyring(oldKey)
	require.NoError(t, err)
	sealed, err := oldCipher.Encrypt([]byte("JBSWY3DPEHPK3PXP"))
	require.NoError(t, err)

	ring, err := NewCipherKeyring(newKey, oldKey)
	require.NoError(t, err)
	plaintext, err := ring.Decrypt(sealed)

	require.NoError(t, err)
	assert.Equal(t, "JBSWY3DPEHPK3PXP", string(plaintext))
}

func TestKeyringEncryptsWithPrimaryOnly(t *testing.T) {
	oldKey := []byte(strings.Repeat("a", 32))
	newKey := []byte(strings.Repeat("b", 32))
	ring, err := NewCipherKeyring(newKey, oldKey)
	require.NoError(t, err)
	oldOnly, err := NewCipherKeyring(oldKey)
	require.NoError(t, err)
	newOnly, err := NewCipherKeyring(newKey)
	require.NoError(t, err)

	sealed, err := ring.Encrypt([]byte("secret"))
	require.NoError(t, err)

	_, err = oldOnly.Decrypt(sealed)
	assert.Error(t, err, "fresh ciphertext must be sealed under the primary key")
	_, err = newOnly.Decrypt(sealed)
	assert.NoError(t, err)
}

func TestKeyringPendingKeyDerivesFromPrimary(t *testing.T) {
	oldKey := []byte(strings.Repeat("a", 32))
	newKey := []byte(strings.Repeat("b", 32))
	ring, err := NewCipherKeyring(newKey, oldKey)
	require.NoError(t, err)
	newOnly, err := NewCipherKeyring(newKey)
	require.NoError(t, err)
	oldOnly, err := NewCipherKeyring(oldKey)
	require.NoError(t, err)

	assert.Equal(t, newOnly.PendingKey(), ring.PendingKey())
	assert.NotEqual(t, oldOnly.PendingKey(), ring.PendingKey())
}

func TestPendingKeyIsDerivedAndStable(t *testing.T) {
	c1, err := NewCipher(testHexKey)
	require.NoError(t, err)
	c2, err := NewCipher(testHexKey)
	require.NoError(t, err)

	key := c1.PendingKey()

	assert.Len(t, key, 32)
	assert.Equal(t, key, c2.PendingKey(), "same input key must derive same pending key")
	assert.NotEqual(t, testHexKey, string(key))
}

func TestGenerateKey(t *testing.T) {
	key, err := GenerateKey("ggscale control panel", "op@example.com")

	require.NoError(t, err)
	assert.Equal(t, "ggscale control panel", key.Issuer())
	assert.Equal(t, "op@example.com", key.AccountName())
	assert.NotEmpty(t, key.Secret())
}

func TestKeyFromPartsMatchesGeneratedKey(t *testing.T) {
	generated, err := GenerateKey("ggscale control panel", "op@example.com")
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)

	rebuilt, err := KeyFromParts("ggscale control panel", "op@example.com", generated.Secret())

	require.NoError(t, err)
	assert.Equal(t, generated.Secret(), rebuilt.Secret())
	assert.Equal(t, generated.Issuer(), rebuilt.Issuer())
	assert.Equal(t, generated.AccountName(), rebuilt.AccountName())
	code := totpCodeAt(t, rebuilt.Secret(), now)
	_, ok := ValidateCode(generated.Secret(), code, now)
	assert.True(t, ok)
}

func totpCodeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	require.NoError(t, err)
	return code
}

func TestValidateCode(t *testing.T) {
	key, err := GenerateKey("ggscale", "player@example.com")
	require.NoError(t, err)
	secret := key.Secret()
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / 30

	// ValidateCode only performs crypto/window validation; replay rejection is
	// owned by the atomic last_used_step store update, not this function.
	tests := []struct {
		name     string
		code     string
		wantStep int64
		wantOK   bool
	}{
		{name: "should_accept_current_step", code: totpCodeAt(t, secret, now), wantStep: step, wantOK: true},
		{name: "should_accept_previous_step", code: totpCodeAt(t, secret, now.Add(-30*time.Second)), wantStep: step - 1, wantOK: true},
		{name: "should_accept_next_step", code: totpCodeAt(t, secret, now.Add(30*time.Second)), wantStep: step + 1, wantOK: true},
		{name: "should_reject_two_steps_back", code: totpCodeAt(t, secret, now.Add(-60*time.Second))},
		{name: "should_reject_two_steps_forward", code: totpCodeAt(t, secret, now.Add(60*time.Second))},
		{name: "should_reject_wrong_code", code: "000000"},
		{name: "should_reject_short_code", code: "12345"},
		{name: "should_reject_non_digit_code", code: "abcdef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStep, ok := ValidateCode(secret, tt.code, now)

			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantStep, gotStep)
			}
		})
	}
}

func TestValidateCodeTrimsSpaces(t *testing.T) {
	key, err := GenerateKey("ggscale", "player@example.com")
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)
	code := totpCodeAt(t, key.Secret(), now)

	_, ok := ValidateCode(key.Secret(), " "+code+" ", now)

	assert.True(t, ok)
}

func TestGenerateBackupCodes(t *testing.T) {
	codes, err := GenerateBackupCodes()

	require.NoError(t, err)
	assert.Len(t, codes, BackupCodeCount)
	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		assert.Regexp(t, `^[a-z2-7]{5}-[a-z2-7]{5}$`, code)
		assert.False(t, seen[code], "codes must be unique")
		seen[code] = true
	}
}

func TestHashBackupCodeNormalizes(t *testing.T) {
	base := HashBackupCode("abc23-def45")

	tests := []struct {
		name  string
		input string
		same  bool
	}{
		{name: "should_match_uppercase_input", input: "ABC23-DEF45", same: true},
		{name: "should_match_without_hyphen", input: "abc23def45", same: true},
		{name: "should_match_with_spaces", input: " abc23 def45 ", same: true},
		{name: "should_differ_for_other_code", input: "abc23-def46"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HashBackupCode(tt.input)

			assert.Equal(t, tt.same, string(base) == string(got))
		})
	}
}

func TestIsTOTPCode(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "should_accept_six_digits", input: "123456", want: true},
		{name: "should_accept_padded_six_digits", input: " 123456 ", want: true},
		{name: "should_reject_five_digits", input: "12345"},
		{name: "should_reject_seven_digits", input: "1234567"},
		{name: "should_reject_backup_code_shape", input: "abc23-def45"},
		{name: "should_reject_empty", input: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsTOTPCode(tt.input))
		})
	}
}

func TestQRPNGDataURI(t *testing.T) {
	key, err := GenerateKey("ggscale", "player@example.com")
	require.NoError(t, err)

	uri, err := QRPNGDataURI(key)

	require.NoError(t, err)
	require.True(t, strings.HasPrefix(uri, "data:image/png;base64,"))
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, "data:image/png;base64,"))
	require.NoError(t, err)
	assert.Equal(t, "\x89PNG", string(raw[:4]))
}

func TestPendingRoundTrip(t *testing.T) {
	key := []byte("test-key-32-bytes-long-aaaaaaaaa")
	now := time.Unix(1_700_000_000, 0)
	p := Pending{Subject: "42", Email: "op@example.com", ExpiresAt: now.Add(PendingTTL).Unix()}

	raw := EncodePending(key, p)
	got, ok := DecodePending(key, raw, now)

	require.True(t, ok)
	assert.Equal(t, "42", got.Subject)
	assert.Equal(t, "op@example.com", got.Email)
}

func TestDecodePendingRejectsBadInput(t *testing.T) {
	key := []byte("test-key-32-bytes-long-aaaaaaaaa")
	now := time.Unix(1_700_000_000, 0)
	valid := EncodePending(key, Pending{Subject: "42", ExpiresAt: now.Add(time.Minute).Unix()})
	expired := EncodePending(key, Pending{Subject: "42", ExpiresAt: now.Add(-time.Minute).Unix()})
	otherKey := EncodePending([]byte("another-key-32-bytes-long-aaaaaa"), Pending{Subject: "42", ExpiresAt: now.Add(time.Minute).Unix()})

	tests := []struct {
		name string
		raw  string
	}{
		{name: "should_reject_tampered_payload", raw: "x" + valid[1:]},
		{name: "should_reject_wrong_key", raw: otherKey},
		{name: "should_reject_expired", raw: expired},
		{name: "should_reject_garbage", raw: "not-a-cookie"},
		{name: "should_reject_empty", raw: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := DecodePending(key, tt.raw, now)

			assert.False(t, ok)
		})
	}
}

func TestDecodePendingRejectsForeignPurpose(t *testing.T) {
	// A signed payload whose purpose is not the 2FA tag must not open,
	// even under the right key — e.g. an email-verify cookie replayed
	// into the 2FA cookie slot.
	key := []byte("test-key-32-bytes-long-aaaaaaaaa")
	now := time.Unix(1_700_000_000, 0)
	p := Pending{Purpose: "verify", Subject: "42", ExpiresAt: now.Add(time.Minute).Unix()}
	raw := encodePendingRaw(key, p)

	_, ok := DecodePending(key, raw, now)

	assert.False(t, ok)
}

func TestGroupSecret(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		want   string
	}{
		{name: "should_group_in_fours", secret: "ABCDEFGH", want: "ABCD EFGH"},
		{name: "should_keep_trailing_partial_group", secret: "ABCDEF", want: "ABCD EF"},
		{name: "should_pass_short_secret_through", secret: "ABC", want: "ABC"},
		{name: "should_handle_empty", secret: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, GroupSecret(tt.secret))
		})
	}
}
