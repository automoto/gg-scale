package httpapi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The recipient, negotiation_id, and kind constraints are declared as huma
// struct tags on gameSessionSignalRequest and enforced by the schema layer
// (covered by the integration tests). Only the payload byte cap — which JSON
// Schema's rune-counting maxLength cannot express — is validated in Go, so
// that is what these unit tests exercise.

func TestValidateSignalPayloadSize_rejectsEmpty(t *testing.T) {
	assert.Error(t, validateSignalPayloadSize(""))
}

func TestValidateSignalPayloadSize_rejectsOversized(t *testing.T) {
	assert.Error(t, validateSignalPayloadSize(strings.Repeat("x", maxGameSessionSignalBytes+1)))
}

func TestValidateSignalPayloadSize_acceptsWithinBounds(t *testing.T) {
	assert.NoError(t, validateSignalPayloadSize("v=0"))
	assert.NoError(t, validateSignalPayloadSize(strings.Repeat("x", maxGameSessionSignalBytes)))
}
