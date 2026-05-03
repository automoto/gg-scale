package dashboard

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidDashboardEmail(t *testing.T) {
	tests := []struct {
		email string
		valid bool
	}{
		{"user@example.com", true},
		{"user+tag@sub.example.com", true},
		{"", false},
		{"notanemail", false},
		{"a@b.c", true},
		{"@nodomain.com", false},
		{"noatsign.com", false},
		{"spaces in@email.com", false},
		{"user@", false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.valid, validDashboardEmail(tc.email), "email: %q", tc.email)
	}
}
