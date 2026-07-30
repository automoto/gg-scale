package httpapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWSConnState_check_passes_while_verified(t *testing.T) {
	st := &wsConnState{lastVerified: time.Now()}
	assert.NoError(t, st.check(time.Minute))
}

func TestWSConnState_check_fails_once_marked_stale(t *testing.T) {
	st := &wsConnState{lastVerified: time.Now()}
	st.markStale()
	assert.ErrorIs(t, st.check(time.Minute), errRealtimeSessionRevoked)
}

func TestWSConnState_check_fails_closed_past_grace(t *testing.T) {
	// Sweeps failing (infrastructure) stop advancing lastVerified; the
	// bounded grace must close the connection rather than fail open forever.
	st := &wsConnState{lastVerified: time.Now().Add(-2 * time.Minute)}
	assert.ErrorIs(t, st.check(time.Minute), errRealtimeUnverifiable)
}

func TestWSConnState_markVerified_never_revives_a_stale_connection(t *testing.T) {
	st := &wsConnState{lastVerified: time.Now().Add(-time.Hour)}
	st.markStale()
	st.markVerified(time.Now())
	assert.ErrorIs(t, st.check(time.Minute), errRealtimeSessionRevoked)
}
