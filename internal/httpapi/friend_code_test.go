package httpapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFriendCode(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		code, err := newFriendCode()
		require.NoError(t, err)
		assert.Len(t, code, friendCodeLen)
		for _, r := range code {
			assert.Contains(t, friendCodeAlphabet, string(r),
				"codes must avoid ambiguous characters")
		}
		seen[code] = true
	}
	assert.Greater(t, len(seen), 45, "codes must not repeat in practice")
}

func TestNormalizeFriendCode(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"already_clean", "XKCD4242", "XKCD4242"},
		{"lowercase", "xkcd4242", "XKCD4242"},
		{"dashed", "XKCD-4242", "XKCD4242"},
		{"spaced_and_mixed", " xkcd 4242 ", "XKCD4242"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, normalizeFriendCode(tt.in))
		})
	}
}

func TestFriendCodeAlphabet_has_no_ambiguous_runes(t *testing.T) {
	for _, r := range "IO01" {
		assert.False(t, strings.ContainsRune(friendCodeAlphabet, r))
	}
}
