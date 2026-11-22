package projectadmin

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHeaderSafeName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain name kept", "Doomerang", "Doomerang"},
		{"unicode kept", "Süper Güme 🎮", "Süper Güme 🎮"},
		{"newline falls back", "Evil\nGame", "fallback"},
		{"carriage return falls back", "Evil\rBcc: x", "fallback"},
		{"empty falls back", "", "fallback"},
		{"oversized falls back", strings.Repeat("a", 200), "fallback"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HeaderSafeName(tc.in, "fallback"))
		})
	}
}
