package dashboard

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
