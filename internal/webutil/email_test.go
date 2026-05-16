package webutil_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/webutil"
)

func TestValidateEmailAccepts(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"alice@example.com", "alice@example.com"},
		{"  alice@example.com  ", "alice@example.com"},
		{"alice@EXAMPLE.com", "alice@example.com"},
		{"a+filter@b.co", "a+filter@b.co"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			out, err := webutil.ValidateEmail(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
		})
	}
}

func TestValidateEmailRejects(t *testing.T) {
	tests := []string{
		"",
		"a@b",
		"alice",
		"alice@",
		"@example.com",
		"alice@example.com\r\nBcc: evil@x",
		"alice\nbob@example.com",
		"alice\x00@example.com",
	}
	for _, in := range tests {
		t.Run(in, func(t *testing.T) {
			_, err := webutil.ValidateEmail(in)
			assert.ErrorIs(t, err, webutil.ErrInvalidEmail)
		})
	}
}
