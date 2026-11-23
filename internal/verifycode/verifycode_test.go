package verifycode

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateCode_six_digits(t *testing.T) {
	code, err := GenerateCode()
	require.NoError(t, err)
	assert.Len(t, code, 6)
	for _, r := range code {
		assert.True(t, r >= '0' && r <= '9')
	}
}

func TestGenerateCode_distribution(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		code, err := GenerateCode()
		require.NoError(t, err)
		seen[code] = struct{}{}
	}
	assert.Greater(t, len(seen), 190)
}

func TestGenerateInviteCode_urlsafe(t *testing.T) {
	c, err := GenerateInviteCode()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(c), 32)
	for _, r := range c {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		assert.True(t, ok, "expected URL-safe char, got %q", c)
	}
}

func TestNewSalt_length(t *testing.T) {
	s, err := NewSalt()
	require.NoError(t, err)
	assert.Len(t, s, saltBytes)
}

func TestHashToken_should_match_the_stored_link_hash(t *testing.T) {
	want := sha256.Sum256([]byte(":abc"))

	assert.Equal(t, want[:], HashToken("abc"))
}

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestHashCode_should_be_deterministic(t *testing.T) {
	assert.Equal(t, HashCode(testKey, []byte("abc"), "123456"), HashCode(testKey, []byte("abc"), "123456"))
}

func TestHashCode_should_change_with_salt(t *testing.T) {
	assert.NotEqual(t, HashCode(testKey, []byte("a"), "123456"), HashCode(testKey, []byte("b"), "123456"))
}

func TestHashCode_should_change_with_code(t *testing.T) {
	assert.NotEqual(t, HashCode(testKey, []byte("abc"), "123456"), HashCode(testKey, []byte("abc"), "654321"))
}

func TestHashCode_should_change_with_key(t *testing.T) {
	other := []byte("fedcba9876543210fedcba9876543210")

	assert.NotEqual(t, HashCode(testKey, []byte("abc"), "123456"), HashCode(other, []byte("abc"), "123456"))
}

func TestHashCode_should_not_be_a_plain_hash(t *testing.T) {
	plain := sha256.Sum256([]byte("abc:123456"))

	assert.NotEqual(t, plain[:], HashCode(testKey, []byte("abc"), "123456"))
}

func TestCanResend(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		last  time.Time
		allow bool
	}{
		{"never_sent", time.Time{}, true},
		{"30s_ago", now.Add(-30 * time.Second), false},
		{"exactly_cooldown", now.Add(-ResendCooldown), true},
		{"2m_ago", now.Add(-2 * time.Minute), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.allow, CanResend(tc.last, now))
		})
	}
}

func TestExpired(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		expires time.Time
		want    bool
	}{
		{"future", now.Add(time.Minute), false},
		{"now", now, true},
		{"past", now.Add(-time.Second), true},
		{"zero", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Expired(tc.expires, now))
		})
	}
}

func TestAttemptsExhausted(t *testing.T) {
	tests := []struct {
		n    int
		want bool
	}{
		{0, false},
		{4, false},
		{5, true},
		{99, true},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, AttemptsExhausted(tc.n), "n=%d", tc.n)
	}
}

func TestLifetimeExhausted(t *testing.T) {
	tests := []struct {
		n    int
		want bool
	}{
		{0, false},
		{MaxLifetimeAttempts - 1, false},
		{MaxLifetimeAttempts, true},
		{MaxLifetimeAttempts + 1, true},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, LifetimeExhausted(tc.n), "n=%d", tc.n)
	}
}

func TestAccountLocked(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		lockedUntil time.Time
		want        bool
	}{
		{"zero", time.Time{}, false},
		{"past", now.Add(-time.Second), false},
		{"now", now, false},
		{"future", now.Add(time.Second), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AccountLocked(tc.lockedUntil, now))
		})
	}
}

func TestConstants_match_plan(t *testing.T) {
	assert.Equal(t, 15*time.Minute, CodeTTL)
	assert.Equal(t, 3*24*time.Hour, InviteTTL)
	assert.Equal(t, time.Minute, ResendCooldown)
	assert.Equal(t, 5, MaxAttempts)
}
