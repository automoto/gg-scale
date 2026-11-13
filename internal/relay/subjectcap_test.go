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

func TestSubjectAllocCaps_should_keep_cap_of_valid_credential_after_last_allocation_closes(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	caps.now = func() int64 { return 1000 }
	caps.setTenantCap(1, 1300, ptr(1))
	require.NoError(t, caps.acquire(1, 1))
	caps.release(1, 1)
	require.NoError(t, caps.acquire(1, 2))

	assert.ErrorIs(t, caps.acquire(1, 3), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_drop_expired_idle_caps_when_new_tenant_authenticates(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	now := int64(1000)
	caps.now = func() int64 { return now }
	caps.setTenantCap(1, 1300, ptr(1))
	require.NoError(t, caps.acquire(1, 1))
	caps.release(1, 1)
	now = 2000

	caps.setTenantCap(2, 2300, ptr(5))

	assert.NotContains(t, caps.tenantCap, int64(1))
}

// The race from review: OnAuth records the cap just before the credential
// expires, the tenant's last old allocation closes and another tenant triggers
// cleanup after expiry, then the same request reaches AllocatePacketConn.
func TestSubjectAllocCaps_should_keep_cap_for_allocate_that_authenticated_just_before_expiry(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	now := int64(900)
	caps.now = func() int64 { return now }
	caps.setTenantCap(1, 900, ptr(5))
	require.NoError(t, caps.acquire(1, 1))
	now = 1000
	caps.setTenantCap(1, 1000, ptr(0))
	now = 1001
	caps.release(1, 1)
	caps.setTenantCap(2, 1301, ptr(5))

	assert.ErrorIs(t, caps.acquire(1, 2), errTenantAllocationCap)
}

func TestSubjectAllocCaps_should_keep_idle_cap_within_retention_after_expiry(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	now := int64(1000)
	caps.now = func() int64 { return now }
	caps.setTenantCap(1, 1000, ptr(1))
	now = 1000 + capRetentionSeconds

	caps.setTenantCap(2, now+300, ptr(5))

	assert.Contains(t, caps.tenantCap, int64(1))
}

func TestSubjectAllocCaps_should_drop_idle_cap_just_after_retention(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	now := int64(1000)
	caps.now = func() int64 { return now }
	caps.setTenantCap(1, 1000, ptr(1))
	now = 1000 + capRetentionSeconds + 1

	caps.setTenantCap(2, now+300, ptr(5))

	assert.NotContains(t, caps.tenantCap, int64(1))
}

func TestSubjectAllocCaps_should_keep_caps_of_tenants_with_live_allocations(t *testing.T) {
	caps := newSubjectAllocCaps(0, 2000)
	now := int64(1000)
	caps.now = func() int64 { return now }
	caps.setTenantCap(1, 1300, ptr(1))
	require.NoError(t, caps.acquire(1, 1))
	now = 2000

	caps.setTenantCap(2, 2300, ptr(5))

	assert.Contains(t, caps.tenantCap, int64(1))
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
