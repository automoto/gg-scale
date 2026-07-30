package gamesession

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEffectiveState(t *testing.T) {
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	tests := []struct {
		name      string
		state     string
		expiresAt time.Time
		want      string
	}{
		{"open_live", "open", future, "open"},
		{"open_expired", "open", past, "expired"},
		{"in_progress_expired", "in_progress", past, "expired"},
		{"ended_stays_ended", "ended", past, "ended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, EffectiveState(tt.state, tt.expiresAt))
		})
	}
}

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
