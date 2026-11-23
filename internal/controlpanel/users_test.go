package controlpanel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidControlPanelEmail(t *testing.T) {
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
		// The shared validator's RFC 5321 cap: an oversized address must be
		// rejected before it can reach the durable job queue.
		{strings.Repeat("a", 250) + "@example.com", false},
		// Display-name form is an address field, not a mailbox.
		{"Alice <alice@example.com>", false},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.valid, validControlPanelEmail(tc.email), "email: %q", tc.email)
	}
}
