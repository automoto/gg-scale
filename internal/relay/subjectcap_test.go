package relay

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubjectAllocCaps_should_refuse_player_over_cap(t *testing.T) {
	caps := newSubjectAllocCaps(2, 0)
	require.NoError(t, caps.acquire(1, 42))
	require.NoError(t, caps.acquire(1, 42))

	err := caps.acquire(1, 42)

	assert.ErrorIs(t, err, errPlayerAllocationCap)
}

func TestSubjectAllocCaps_should_count_player_refusals(t *testing.T) {
	caps := newSubjectAllocCaps(1, 0)
	require.NoError(t, caps.acquire(1, 42))

	_ = caps.check(1, 42)
	_ = caps.acquire(1, 42)

	assert.Equal(t, int64(2), caps.playerRejected.Load())
}

func TestSubjectAllocCaps_should_admit_player_again_after_release(t *testing.T) {
	caps := newSubjectAllocCaps(1, 0)
	require.NoError(t, caps.acquire(1, 42))

	caps.release(1, 42)

	assert.NoError(t, caps.acquire(1, 42))
}

func TestSubjectAllocCaps_should_keep_players_independent(t *testing.T) {
	caps := newSubjectAllocCaps(1, 0)
	require.NoError(t, caps.acquire(1, 42))

	assert.NoError(t, caps.acquire(1, 43))
}

func TestSubjectAllocCaps_should_use_default_tenant_cap_without_credential_cap(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2)
	require.NoError(t, caps.acquire(1, 1))
	require.NoError(t, caps.acquire(1, 2))

	err := caps.acquire(1, 3)

	assert.ErrorIs(t, err, errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_use_tenant_cap_from_credential(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 1)
	require.NoError(t, caps.acquire(1, 1))

	err := caps.acquire(1, 2)

	assert.ErrorIs(t, err, errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_fall_back_to_default_when_credential_has_no_cap(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 1)
	caps.setTenantCap(1, 0)
	require.NoError(t, caps.acquire(1, 1))

	assert.NoError(t, caps.acquire(1, 2))
}

func TestSubjectAllocCaps_should_count_tenant_refusals(t *testing.T) {
	caps := newSubjectAllocCaps(0, 1)
	require.NoError(t, caps.acquire(1, 1))

	_ = caps.acquire(1, 2)

	assert.Equal(t, int64(1), caps.tenantRejected.Load())
}

func TestSubjectAllocCaps_should_not_charge_tenant_when_player_is_refused(t *testing.T) {
	caps := newSubjectAllocCaps(1, 2)
	require.NoError(t, caps.acquire(1, 1))
	_ = caps.acquire(1, 1)

	assert.NoError(t, caps.acquire(1, 2), "the refused allocation left the tenant count at one")
}

func TestSubjectAllocCaps_should_check_without_reserving(t *testing.T) {
	caps := newSubjectAllocCaps(1, 0)
	require.NoError(t, caps.check(1, 42))

	assert.NoError(t, caps.acquire(1, 42))
}

func TestSubjectAllocCaps_should_forget_subjects_with_no_live_allocations(t *testing.T) {
	caps := newSubjectAllocCaps(32, 2000)
	require.NoError(t, caps.acquire(1, 42))

	caps.release(1, 42)

	assert.Equal(t, [2]int{0, 0}, [2]int{len(caps.players), len(caps.tenants)})
}

func TestSubjectAllocCaps_should_be_unlimited_when_caps_are_zero(t *testing.T) {
	caps := newSubjectAllocCaps(0, 0)
	for range 100 {
		require.NoError(t, caps.acquire(1, 42))
	}

	assert.NoError(t, caps.acquire(1, 42))
}

func TestAuthEvent_should_apply_tenant_cap_from_verified_credential(t *testing.T) {
	iss := NewIssuer("shared-secret", "ggscale", time.Minute)
	iss.SetTenantCapInCredentials(true)
	s := &Server{caps: newSubjectAllocCaps(0, 2000)}
	creds, err := iss.IssueWithTenantCap(7, 1, 1)
	require.NoError(t, err)

	s.authEventHandler(iss).OnAuth(nil, nil, "UDP", creds.Username, "ggscale", "Allocate", true)

	require.NoError(t, s.caps.acquire(7, 1))
	assert.ErrorIs(t, s.caps.acquire(7, 2), errTenantAllocationCap)
}

func TestAuthEvent_should_ignore_tenant_cap_when_integrity_fails(t *testing.T) {
	iss := NewIssuer("shared-secret", "ggscale", time.Minute)
	iss.SetTenantCapInCredentials(true)
	s := &Server{caps: newSubjectAllocCaps(0, 1)}
	forged, err := iss.IssueWithTenantCap(7, 1, 999999)
	require.NoError(t, err)

	s.authEventHandler(iss).OnAuth(nil, nil, "UDP", forged.Username, "ggscale", "Allocate", false)

	require.NoError(t, s.caps.acquire(7, 1))
	assert.ErrorIs(t, s.caps.acquire(7, 2), errTenantAllocationCap)
}
