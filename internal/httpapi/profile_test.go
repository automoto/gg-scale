package httpapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidPrintableName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"simple", "Nova Fox", true},
		{"unicode", "Ñova★", true},
		{"max_length", strings.Repeat("x", 64), true},
		{"empty", "", false},
		{"too_long", strings.Repeat("x", 65), false},
		{"control_char", "bad\aname", false},
		{"newline", "line\nbreak", false},
		{"tab", "tab\there", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, validPrintableName(tc.in, displayNameMaxChars))
		})
	}
}
