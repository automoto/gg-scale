package main

import (
	"testing"

	"github.com/automoto/gg-scale/internal/relay"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegisterRelayServerMetricsExposesEveryRelayCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	registerRelayServerMetrics(reg, &relay.Server{})

	families, err := reg.Gather()
	require.NoError(t, err)

	names := make(map[string]bool, len(families))
	for _, f := range families {
		names[f.GetName()] = true
	}

	want := []string{
		"ggscale_relay_active_allocations",
		"ggscale_relay_allocations_rejected_total",
		"ggscale_relay_auth_failures_total",
		"ggscale_relay_alloc_throttled_total",
		"ggscale_relay_peer_rejected_total",
		"ggscale_relay_bandwidth_dropped_packets_total",
	}
	for _, name := range want {
		assert.True(t, names[name], "%s is registered", name)
	}
}

func TestRegisterRelayServerMetrics_should_label_rejections_by_reason(t *testing.T) {
	reg := prometheus.NewRegistry()
	registerRelayServerMetrics(reg, &relay.Server{})

	families, err := reg.Gather()
	require.NoError(t, err)

	var reasons []string
	for _, f := range families {
		if f.GetName() != "ggscale_relay_allocations_rejected_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" {
					reasons = append(reasons, l.GetValue())
				}
			}
		}
	}
	assert.ElementsMatch(t, []string{"node_cap", "player_cap", "tenant_cap"}, reasons)
}
