package relay

import (
	"strconv"
	"strings"
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
	caps.setTenantCap(1, 100, ptr(1))
	require.NoError(t, caps.acquire(1, 1))

	err := caps.acquire(1, 2)

	assert.ErrorIs(t, err, errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_keep_recorded_cap_when_credential_has_no_cap(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 100, ptr(1))
	caps.setTenantCap(1, 200, nil)
	require.NoError(t, caps.acquire(1, 1))

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_ignore_cap_from_older_credential(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 200, ptr(1))
	caps.setTenantCap(1, 100, ptr(2000))
	require.NoError(t, caps.acquire(1, 1))

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_apply_cap_from_newer_credential(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 100, ptr(2000))
	caps.setTenantCap(1, 200, ptr(1))
	require.NoError(t, caps.acquire(1, 1))

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

// The race from review: OnAuth records the cap, the tenant's last old
// allocation closes and other tenants authenticate, then the same request
// reaches AllocatePacketConn. Nothing in between may remove the cap.
func TestSubjectAllocCaps_should_keep_cap_through_release_and_other_tenants(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 900, ptr(5))
	require.NoError(t, caps.acquire(1, 1))
	caps.setTenantCap(1, 1000, ptr(0))
	caps.release(1, 1)
	for tenant := int64(2); tenant < 100; tenant++ {
		caps.setTenantCap(tenant, 1_000_000, ptr(5))
	}

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_keep_stricter_cap_from_credential_with_same_expiry(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 100, ptr(1))
	caps.setTenantCap(1, 100, ptr(2000))
	require.NoError(t, caps.acquire(1, 1))

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_apply_stricter_cap_from_credential_with_same_expiry(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 100, ptr(2000))
	caps.setTenantCap(1, 100, ptr(1))
	require.NoError(t, caps.acquire(1, 1))

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_treat_unlimited_as_loosest_cap_with_same_expiry(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 100, ptr(1))
	caps.setTenantCap(1, 100, ptr(-1))
	require.NoError(t, caps.acquire(1, 1))

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

func TestIssuer_tenantAllocCap_should_return_credential_expiry(t *testing.T) {
	iss := NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	iss.SetTenantCapInCredentials(true)
	creds, err := iss.IssueWithTenantCap(1, 2, ptr(5))
	require.NoError(t, err)
	want, err := strconv.ParseInt(strings.SplitN(creds.Username, ":", 2)[0], 10, 64)
	require.NoError(t, err)

	_, expires, _, ok := iss.tenantAllocCap(creds.Username)

	require.True(t, ok)
	assert.Equal(t, want, expires)
}

func TestSubjectAllocCaps_should_refuse_tenant_with_zero_cap(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.setTenantCap(1, 100, ptr(0))

	assert.ErrorIs(t, caps.acquire(1, 1), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_not_cap_tenant_with_unlimited_cap(t *testing.T) {
	caps := newSubjectAllocCaps(0, 1)
	caps.setTenantCap(1, 100, ptr(-1))
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
	creds, err := iss.IssueWithTenantCap(7, 1, ptr(1))
	require.NoError(t, err)

	s.authEventHandler(iss).OnAuth(nil, nil, "UDP", creds.Username, "ggscale", "Allocate", true)

	require.NoError(t, s.caps.acquire(7, 1))
	assert.ErrorIs(t, s.caps.acquire(7, 2), errTenantAllocationCap)
}

func TestAuthEvent_should_ignore_tenant_cap_when_integrity_fails(t *testing.T) {
	iss := NewIssuer("shared-secret", "ggscale", time.Minute)
	iss.SetTenantCapInCredentials(true)
	s := &Server{caps: newSubjectAllocCaps(0, 1)}
	forged, err := iss.IssueWithTenantCap(7, 1, ptr(999999))
	require.NoError(t, err)

	s.authEventHandler(iss).OnAuth(nil, nil, "UDP", forged.Username, "ggscale", "Allocate", false)

	require.NoError(t, s.caps.acquire(7, 1))
	assert.ErrorIs(t, s.caps.acquire(7, 2), errTenantAllocationCap)
}

func ptr(n int64) *int64 { return &n }

// AuthHandler checks expiry, then pion calls OnAuth. If the second ticks over in
// between, the verified credential's cap must still be recorded.
func TestIssuer_tenantAllocCap_should_read_cap_when_credential_expired_after_auth(t *testing.T) {
	iss := NewIssuer(strings.Repeat("s", 32), "ggscale", time.Minute)
	iss.SetTenantCapInCredentials(true)
	creds, err := iss.IssueWithTenantCap(1, 2, ptr(0))
	require.NoError(t, err)
	expires, err := strconv.ParseInt(strings.SplitN(creds.Username, ":", 2)[0], 10, 64)
	require.NoError(t, err)
	iss.now = func() time.Time { return time.Unix(expires+1, 0) }

	_, _, allocCap, ok := iss.tenantAllocCap(creds.Username)

	require.True(t, ok)
	assert.Equal(t, int64(0), *allocCap)
}
