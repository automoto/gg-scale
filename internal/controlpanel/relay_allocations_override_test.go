package controlpanel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	sqlcgen "github.com/automoto/gg-scale/internal/db/sqlc"
	"github.com/automoto/gg-scale/internal/quota"
	"github.com/automoto/gg-scale/internal/tenant"
)

func TestIsQuotaAxis_should_not_let_tenants_request_relay_allocations(t *testing.T) {
	assert.False(t, isQuotaAxis(quota.AxisRelayAllocations))
}

func TestIsAdminQuotaAxis_should_let_platform_admins_set_relay_allocations(t *testing.T) {
	assert.True(t, isAdminQuotaAxis(quota.AxisRelayAllocations))
}

func TestIsAdminQuotaAxis_should_include_every_requestable_axis(t *testing.T) {
	for _, a := range quotaOverrideAxes {
		assert.True(t, isAdminQuotaAxis(a.Value), a.Value)
	}
}

func TestApplyChangeRequest_should_refuse_relay_allocations_override(t *testing.T) {
	axis := quota.AxisRelayAllocations
	limit := int64(5000)
	req := sqlcgen.GetTenantChangeRequestByIDRow{Kind: changeKindQuotaOverride, Feature: &axis, RequestedLimit: &limit}

	err := (&Handler{}).applyChangeRequest(context.Background(), nil, req, 1)

	assert.ErrorIs(t, err, errNotRequestableAxis)
}

func TestBuildQuotaOverrideRows_should_list_relay_allocations_with_class_default(t *testing.T) {
	rows := buildQuotaOverrideRows(tenant.Tier0, nil)

	var got string
	for _, r := range rows {
		if r.Axis == quota.AxisRelayAllocations {
			got = r.DefaultLabel
		}
	}
	assert.Equal(t, "200", got)
}

func TestBuildQuotaOverrideRows_should_show_relay_allocations_override(t *testing.T) {
	rows := buildQuotaOverrideRows(tenant.Tier0, map[string]int64{quota.AxisRelayAllocations: 5000})

	var got *int64
	for _, r := range rows {
		if r.Axis == quota.AxisRelayAllocations {
			got = r.Override
		}
	}
	assert.Equal(t, int64(5000), *got)
}
