package controlpanel

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/ratelimit"
)

func TestSetTenantAPIOverride_rejects_partial_zero(t *testing.T) {
	h := &Handler{} // nil pool: validation returns before any DB access
	err := h.setTenantAPIOverride(context.Background(), 1, 2, 5, 0)
	assert.ErrorIs(t, err, errIncompleteLimit, "rate>0, burst=0 must not persist a dead bucket")

	err = h.setTenantAPIOverride(context.Background(), 1, 2, 0, 5)
	assert.ErrorIs(t, err, errIncompleteLimit, "rate=0, burst>0 never refills")
}

func TestSetTenantAPIOverride_rejects_nonfinite(t *testing.T) {
	h := &Handler{}
	err := h.setTenantAPIOverride(context.Background(), 1, 2, math.NaN(), math.NaN())
	assert.ErrorIs(t, err, errInvalidLimit)

	err = h.setTenantAPIOverride(context.Background(), 1, 2, math.Inf(1), math.Inf(1))
	assert.ErrorIs(t, err, errInvalidLimit)
}

func TestSetTenantRecipientInviteOverride_rejects_nonfinite(t *testing.T) {
	h := &Handler{} // nil pool: validation returns before any DB access
	assert.ErrorIs(t, h.setTenantRecipientInviteOverride(context.Background(), 1, 2, math.NaN(), 600), errInvalidLimit)
	assert.ErrorIs(t, h.setTenantRecipientInviteOverride(context.Background(), 1, 2, 3, math.Inf(1)), errInvalidLimit)
}

func TestSetTenantRecipientInviteOverride_rejects_partial_zero(t *testing.T) {
	h := &Handler{} // nil pool: validation returns before any DB access
	// One-sided zero would persist a dead bucket; both-zero (clear) is the only
	// valid way to zero it out.
	assert.ErrorIs(t, h.setTenantRecipientInviteOverride(context.Background(), 1, 2, 3, 0), errIncompleteLimit)
	assert.ErrorIs(t, h.setTenantRecipientInviteOverride(context.Background(), 1, 2, 0, 600), errIncompleteLimit)
}

func TestSetTenantRecipientInviteOverride_rejects_fractional_below_one(t *testing.T) {
	h := &Handler{} // nil pool: validation returns before any DB access
	// A burst in (0,1) can't admit even one send; only 0 (clear) or >=1 is valid.
	assert.ErrorIs(t, h.setTenantRecipientInviteOverride(context.Background(), 1, 2, 0.5, 600), errInvalidLimit)
}

func TestSetTenantRecipientInviteOverride_rejects_fractional_burst(t *testing.T) {
	h := &Handler{} // nil pool: validation returns before any DB access
	// Burst is a whole count of back-to-back sends; a fractional value >= 1
	// (e.g. 1.5) would persist a token-bucket cap that doesn't match the
	// displayed integer, so it is rejected too.
	for _, burst := range []float64{1.5, 2.5, 10.25} {
		assert.ErrorIs(t, h.setTenantRecipientInviteOverride(context.Background(), 1, 2, burst, 600), errInvalidLimit,
			"fractional burst %v must be rejected", burst)
	}
}

func TestCooldownSecsFromRate_roundtrips_without_float_noise(t *testing.T) {
	// The setter stores rate = 1/cooldownSecs; the view derives it back for
	// display, and the rounding must cancel float noise (600, not 599.9999…).
	for _, secs := range []float64{600, 90, 1.5, 10} {
		assert.Equal(t, secs, cooldownSecsFromRate(1.0/secs))
	}
	assert.Equal(t, 0.0, cooldownSecsFromRate(0))
}

func TestSetProjectInviteOverride_rejects_above_tenant_cap(t *testing.T) {
	h := &Handler{} // nil pool: the cap check returns before any DB access
	cap := ratelimit.DefaultInviteLimits

	err := h.setProjectInviteOverride(context.Background(), 1, 2, 3, cap.InviterPerHour+1, 0)
	assert.ErrorIs(t, err, errExceedsCap)

	err = h.setProjectInviteOverride(context.Background(), 1, 2, 3, 0, cap.DomainPerDay+1)
	assert.ErrorIs(t, err, errExceedsCap)
}

func TestSetProjectInviteOverride_rejects_negative(t *testing.T) {
	h := &Handler{}
	err := h.setProjectInviteOverride(context.Background(), 1, 2, 3, -1, 0)
	assert.ErrorIs(t, err, errInvalidLimit)
}

func TestParseLimitField(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"10", 10, false},
		{"0.5", 0.5, false},
		{"-1", 0, true},
		{"abc", 0, true},
		// Non-finite values slip past every downstream guard (NaN compares
		// false against all bounds; Postgres orders NaN highest) and disable
		// the limit entirely, so they must be rejected at parse time.
		{"NaN", 0, true},
		{"nan", 0, true},
		{"Inf", 0, true},
		{"+Inf", 0, true},
		{"-Inf", 0, true},
		{"Infinity", 0, true},
	}
	for _, c := range cases {
		got, err := parseLimitField(c.in)
		if c.wantErr {
			assert.Error(t, err, "input %q", c.in)
			continue
		}
		require.NoError(t, err, "input %q", c.in)
		assert.Equal(t, c.want, got)
	}
}

func TestRLValue_blank_when_zero(t *testing.T) {
	assert.Equal(t, "", rlValue(0))
	assert.Equal(t, "10", rlValue(10))
}
