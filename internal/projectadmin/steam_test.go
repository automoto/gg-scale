package projectadmin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateSteamAppID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"spacewar", "480", false},
		{"large_id", "2358720", false},
		{"empty", "", true},
		{"letters", "48o", true},
		{"negative", "-480", true},
		{"whitespace", " 480", true},
		{"too_long", "12345678901", true},
		{"hex_prefix", "0x1E0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSteamAppID(tt.in)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestValidateSteamKey(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"valid_upper", "0123456789ABCDEF0123456789ABCDEF", false},
		{"valid_lower", "0123456789abcdef0123456789abcdef", false},
		{"empty", "", true},
		{"short", "0123456789ABCDEF", true},
		{"long", "0123456789ABCDEF0123456789ABCDEF00", true},
		{"non_hex", "0123456789ABCDEF0123456789ABCDEG", true},
		{"whitespace", "0123456789ABCDEF0123456789ABCDE ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSteamKey(tt.in)

			assert.Equal(t, tt.wantErr, err != nil)
		})
	}
}
