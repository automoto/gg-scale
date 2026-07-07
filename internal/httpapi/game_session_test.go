package httpapi

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestValidateXUID(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		valid bool
	}{
		{"empty", "", false},
		{"simple", "1234567890", true},
		{"max length", strings.Repeat("a", 64), true},
		{"too long", strings.Repeat("a", 65), false},
		{"control char", "abc\x00def", false},
		{"unicode ok", "プレイヤー", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.valid, validateXUID(tt.in))
		})
	}
}

func TestPresenceStatus_validation_counts_runes_not_bytes(t *testing.T) {
	tests := []struct {
		name   string
		status string
		valid  bool
	}{
		{"simple", "in_match", true},
		{"custom", "watching_replay", true},
		{"empty", "", false},
		{"33 ascii", strings.Repeat("a", 33), false},
		{"32 multibyte", strings.Repeat("あ", 32), true},  // 96 bytes, 32 runes
		{"33 multibyte", strings.Repeat("あ", 33), false}, // 99 bytes, 33 runes
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := utf8.RuneCountInString(tt.status)
			got := n >= 1 && n <= presenceStatusMaxChars
			assert.Equal(t, tt.valid, got)
		})
	}
}
