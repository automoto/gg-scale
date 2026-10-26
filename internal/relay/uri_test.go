package relay_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/relay"
)

func TestValidateTURNURI_should_accept_turn_and_turns(t *testing.T) {
	for _, raw := range []string{
		"turn:relay.example.com:3478?transport=udp",
		"turns:relay.example.com:5349?transport=tcp",
	} {
		t.Run(raw, func(t *testing.T) {
			assert.NoError(t, relay.ValidateTURNURI(raw))
		})
	}
}

func TestValidateTURNURI_should_reject_other_schemes_and_missing_hosts(t *testing.T) {
	for _, raw := range []string{"https://relay.example.com", "stun:relay.example.com:3478", "turn:", ":"} {
		t.Run(raw, func(t *testing.T) {
			assert.Error(t, relay.ValidateTURNURI(raw))
		})
	}
}
