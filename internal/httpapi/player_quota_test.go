package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"

	sqlcgen "github.com/ggscale/ggscale/internal/db/sqlc"
	"github.com/ggscale/ggscale/internal/quota"
)

func TestCheckPlayerQuotaSnapshot(t *testing.T) {
	tests := []struct {
		name    string
		qc      sqlcgen.GetTenantQuotaContextRow
		wantErr bool
	}{
		{"should_pass_when_unenforced_even_over_limit", sqlcgen.GetTenantQuotaContextRow{Tier: 0, EnforceQuotas: false, PlayerCount: 10_000_000}, false},
		{"should_pass_when_under_class_limit", sqlcgen.GetTenantQuotaContextRow{Tier: 0, EnforceQuotas: true, PlayerCount: 99_999}, false},
		{"should_reject_at_class_limit", sqlcgen.GetTenantQuotaContextRow{Tier: 0, EnforceQuotas: true, PlayerCount: 100_000}, true},
		{"should_reject_over_class_limit", sqlcgen.GetTenantQuotaContextRow{Tier: 1, EnforceQuotas: true, PlayerCount: 600_000}, true},
		{"should_pass_unlimited_class", sqlcgen.GetTenantQuotaContextRow{Tier: 3, EnforceQuotas: true, PlayerCount: 50_000_000}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPlayerQuotaSnapshot(tc.qc)
			if tc.wantErr {
				var qe *quota.ErrQuotaExceeded
				assert.ErrorAs(t, err, &qe)
				assert.Equal(t, quota.AxisPlayers, qe.Axis)
				return
			}
			assert.NoError(t, err)
		})
	}
}
