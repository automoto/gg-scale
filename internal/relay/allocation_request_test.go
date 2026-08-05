package relay

import (
	"testing"
	"time"

	"github.com/pion/stun/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocationRequestTrackerChargesEvenPortProbesOnce(t *testing.T) {
	issuer := NewIssuer("shared-secret", "ggscale", time.Minute)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)
	msg := stun.MustBuild(
		stun.TransactionID,
		stun.NewType(stun.MethodAllocate, stun.ClassRequest),
		stun.NewUsername(creds.Username),
	)
	tracker := &allocationRequestTracker{}
	limiter := newPlayerAllocLimiter(6, 1)

	scope := tracker.begin(msg.Raw, issuer)
	require.NotNil(t, scope)
	allowed, first, err := tracker.allow(limiter)
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.True(t, first)
	allowed, first, err = tracker.allow(limiter)
	require.NoError(t, err)
	assert.True(t, allowed, "one Allocate request may call the generator more than once")
	assert.False(t, first, "the repeated generator call reuses the first decision")
	scope.end()

	scope = tracker.begin(msg.Raw, issuer)
	require.NotNil(t, scope)
	t.Cleanup(scope.end)
	allowed, first, err = tracker.allow(limiter)
	require.NoError(t, err)
	assert.False(t, allowed, "the next Allocate request consumes a new token")
	assert.True(t, first)
}

func TestAllocationRequestTrackerRejectsGeneratorWithoutAllocateRequest(t *testing.T) {
	tracker := &allocationRequestTracker{}

	allowed, first, err := tracker.allow(newPlayerAllocLimiter(6, 1))

	assert.False(t, allowed)
	assert.False(t, first)
	assert.ErrorIs(t, err, errAllocationSubjectUnavailable)
}

func TestAllocationScopeReleasesBeforeMatchingResponseWrite(t *testing.T) {
	issuer := NewIssuer("shared-secret", "ggscale", time.Minute)
	creds, err := issuer.Issue(1, 42)
	require.NoError(t, err)
	request := stun.MustBuild(
		stun.TransactionID,
		stun.NewType(stun.MethodAllocate, stun.ClassRequest),
		stun.NewUsername(creds.Username),
	)
	tracker := &allocationRequestTracker{}
	var slot allocationScopeSlot
	slot.replace(tracker.begin(request.Raw, issuer))
	response := stun.MustBuild(
		stun.NewTransactionIDSetter(request.TransactionID),
		stun.NewType(stun.MethodAllocate, stun.ClassErrorResponse),
		&stun.ErrorCodeAttribute{Code: stun.CodeBadRequest},
	)

	slot.clearResponse(response.Raw)

	nextCh := make(chan *allocationRequestScope, 1)
	go func() { nextCh <- tracker.begin(request.Raw, issuer) }()
	select {
	case next := <-nextCh:
		require.NotNil(t, next, "the matching response releases the listener gate")
		next.end()
	case <-time.After(time.Second):
		slot.clear()
		t.Fatal("the matching response did not release the listener gate")
	}
}
