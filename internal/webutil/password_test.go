package webutil_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/automoto/gg-scale/internal/webutil"
)

func TestPasswordMatches(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-password"), bcrypt.MinCost)
	require.NoError(t, err)
	dummy, err := bcrypt.GenerateFromPassword([]byte("dummy"), bcrypt.MinCost)
	require.NoError(t, err)

	tests := []struct {
		name     string
		hash     []byte
		password string
		want     bool
	}{
		{"correct password", hash, "correct-password", true},
		{"wrong password", hash, "wrong-password", false},
		{"no hash, any password", nil, "correct-password", false},
		{"no hash, dummy password", nil, "dummy", false},
		{"no hash, empty password", []byte{}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, webutil.PasswordMatches(tc.hash, dummy, tc.password))
		})
	}
}

func TestPasswordMatches_should_spend_a_compare_when_hash_is_empty(t *testing.T) {
	dummy, err := bcrypt.GenerateFromPassword([]byte("dummy"), 10)
	require.NoError(t, err)
	start := time.Now()
	_ = bcrypt.CompareHashAndPassword(dummy, []byte("password"))
	oneCompare := time.Since(start)

	start = time.Now()
	webutil.PasswordMatches(nil, dummy, "password")
	elapsed := time.Since(start)

	// Quarter, not half: oneCompare is a single sample, and a cold or loaded
	// box makes that first compare the slow one, which raises the bar. The
	// failure this guards against is a deleted dummy compare, which returns
	// in microseconds and misses any margin in this range.
	assert.Greater(t, elapsed, oneCompare/4)
}
