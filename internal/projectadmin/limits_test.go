package projectadmin

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/ratelimit"
)

func TestSetInviteLimits_should_refuse_bad_values_before_any_database_access(t *testing.T) {
	limits := ratelimit.DefaultInviteLimits
	tests := []struct {
		name           string
		inviter, daily float64
		want           error
	}{
		{"negative", -1, 0, ErrInvalidInviteLimit},
		{"nan", math.NaN(), 0, ErrInvalidInviteLimit},
		{"inf", 0, math.Inf(1), ErrInvalidInviteLimit},
		{"inviter above cap", limits.InviterPerHour + 1, 0, ErrInviteAboveCap},
		{"domain above cap", 0, limits.DomainPerDay + 1, ErrInviteAboveCap},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A nil pool would panic if the check came after the write.
			err := SetInviteLimits(context.Background(), nil, nil, 1, 2, tc.inviter, tc.daily, Actor{UserID: 1})

			assert.ErrorIs(t, err, tc.want)
		})
	}
}

func TestStorageBytesFromMB(t *testing.T) {
	tests := []struct {
		name    string
		mb      float64
		want    int64
		wantErr bool
	}{
		{"zero clears", 0, 0, false},
		{"whole", 2, 2097152, false},
		{"fraction", 0.5, 524288, false},
		{"negative", -1, 0, true},
		{"nan", math.NaN(), 0, true},
		{"inf", math.Inf(1), 0, true},
		{"overflow", 1e30, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := StorageBytesFromMB(tc.mb)

			assert.Equal(t, []any{tc.want, tc.wantErr}, []any{got, err != nil})
		})
	}
}
