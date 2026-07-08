package controlpanel

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateVerificationCode_six_digits(t *testing.T) {
	code, err := generateVerificationCode()
	require.NoError(t, err)
	assert.Len(t, code, 6, "code should be 6 chars")
	for _, r := range code {
		assert.True(t, r >= '0' && r <= '9', "code should be numeric, got %q", code)
	}
}

func TestGenerateVerificationCode_not_predictable(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := 0; i < 200; i++ {
		code, err := generateVerificationCode()
		require.NoError(t, err)
		seen[code] = struct{}{}
	}
	// 200 draws from 10^6 should yield very high uniqueness; allow some collisions
	// but assert overwhelmingly distinct.
	assert.Greater(t, len(seen), 190)
}

func TestGenerateInviteCode_url_safe(t *testing.T) {
	code, err := generateInviteCode()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(code), 32)
	for _, r := range code {
		isUnreserved := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		assert.True(t, isUnreserved, "invite code must be URL-safe; got %q", code)
	}
}

func TestHashCode_deterministic_with_salt(t *testing.T) {
	salt := []byte("0123456789abcdef")
	h1 := hashCode(salt, "123456")
	h2 := hashCode(salt, "123456")
	assert.Equal(t, h1, h2)
}

func TestHashCode_different_salt_different_hash(t *testing.T) {
	h1 := hashCode([]byte("aaaa"), "123456")
	h2 := hashCode([]byte("bbbb"), "123456")
	assert.NotEqual(t, h1, h2)
}

func TestHashCode_different_code_different_hash(t *testing.T) {
	salt := []byte("salt")
	h1 := hashCode(salt, "123456")
	h2 := hashCode(salt, "654321")
	assert.NotEqual(t, h1, h2)
}

func TestNewSalt_length(t *testing.T) {
	s, err := newSalt()
	require.NoError(t, err)
	assert.Len(t, s, saltBytes)
}

func TestCanResendCode(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		last  time.Time
		allow bool
	}{
		{"never_sent_zero_time", time.Time{}, true},
		{"too_soon_30s_ago", now.Add(-30 * time.Second), false},
		{"exactly_one_minute_ago", now.Add(-resendCooldown), true},
		{"two_minutes_ago", now.Add(-2 * time.Minute), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.allow, canResendCode(tc.last, now))
		})
	}
}

func TestCodeExpired(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		expires time.Time
		expired bool
	}{
		{"future", now.Add(5 * time.Minute), false},
		{"exactly_now", now, true},
		{"past", now.Add(-1 * time.Second), true},
		{"zero_time_means_expired", time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expired, codeExpired(tc.expires, now))
		})
	}
}

func TestInviteExpired_matches_three_days(t *testing.T) {
	assert.Equal(t, 3*24*time.Hour, inviteTTL)
}

func TestVerifyAttemptsExhausted(t *testing.T) {
	tests := []struct {
		attempts  int
		exhausted bool
	}{
		{0, false},
		{1, false},
		{4, false},
		{5, true},
		{99, true},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.exhausted, verifyAttemptsExhausted(tc.attempts), "attempts=%d", tc.attempts)
	}
}

func TestInviteRole_validation(t *testing.T) {
	tests := []struct {
		role  string
		valid bool
	}{
		{"platform_admin", true},
		{"tenant_admin", true},
		{"tenant_member", true},
		{"owner", false},
		{"", false},
		{"player", false},
		{strings.Repeat("x", 200), false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.valid, validInviteRole(tc.role), "role: %q", tc.role)
	}
}
