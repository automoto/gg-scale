package gamesession

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJoinCode_is_alphabet_and_six_chars(t *testing.T) {
	code, err := newJoinCode()
	assert.NoError(t, err)
	assert.Len(t, code, joinCodeLen)
	for _, c := range code {
		assert.Contains(t, joinCodeAlphabet, string(c), "unexpected char %q in join code", c)
	}
}
