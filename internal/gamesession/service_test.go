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

func TestIsMatchmade(t *testing.T) {
	tests := []struct {
		name  string
		props string
		want  bool
	}{
		{"matchmade_true", `{"game_mode": "1v1", "matchmade": true}`, true},
		{"matchmade_false", `{"matchmade": false}`, false},
		{"flag_absent", `{"game_mode": "1v1"}`, false},
		{"empty_props", `{}`, false},
		{"invalid_json", `not json`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsMatchmade([]byte(tt.props)))
		})
	}
}
