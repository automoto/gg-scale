package billing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var handoffKey = []byte("0123456789abcdef0123456789abcdef")

func TestHandoff_sign_verify_round_trip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	token := SignHandoff(handoffKey, 42, DefaultHandoffTTL, now)
	got, err := VerifyHandoff(handoffKey, token, now.Add(time.Minute))

	require.NoError(t, err)
	assert.Equal(t, int64(42), got)
}

func TestHandoff_expired_token_rejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	token := SignHandoff(handoffKey, 42, time.Minute, now)
	_, err := VerifyHandoff(handoffKey, token, now.Add(2*time.Minute))

	assert.Error(t, err)
}

func TestHandoff_wrong_key_rejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	otherKey := []byte("ffffffffffffffffffffffffffffffff")

	token := SignHandoff(handoffKey, 42, DefaultHandoffTTL, now)
	_, err := VerifyHandoff(otherKey, token, now)

	assert.Error(t, err)
}

func TestHandoff_tampered_token_rejected(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	token := SignHandoff(handoffKey, 42, DefaultHandoffTTL, now)
	tampered := []byte(token)
	if tampered[0] == 'A' {
		tampered[0] = 'B'
	} else {
		tampered[0] = 'A'
	}
	_, err := VerifyHandoff(handoffKey, string(tampered), now)

	assert.Error(t, err)
}

func TestHandoff_garbage_rejected(t *testing.T) {
	for _, token := range []string{"", "notatoken", "a.b", "a.b.c"} {
		_, err := VerifyHandoff(handoffKey, token, time.Unix(1_700_000_000, 0))
		assert.Error(t, err, "token=%q", token)
	}
}
