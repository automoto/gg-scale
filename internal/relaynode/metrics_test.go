package relaynode_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/relaynode"
)

func listOf(nodes ...relaynode.Node) func(context.Context) ([]relaynode.Node, error) {
	return func(context.Context) ([]relaynode.Node, error) { return nodes, nil }
}

func TestCollector_should_report_eligibility_and_probe_result_per_known_node(t *testing.T) {
	c := relaynode.NewCollector(listOf(
		relaynode.Node{ID: 3, Region: "us-east", State: relaynode.StateKnown, Eligible: true, LastProbeOK: true},
		relaynode.Node{ID: 4, Region: "us-west", State: relaynode.StateKnown, Eligible: false, LastProbeOK: false},
	))

	expected := `
# HELP ggscale_relay_registry_node_eligible 1 when relay credentials advertise this registered relay node.
# TYPE ggscale_relay_registry_node_eligible gauge
ggscale_relay_registry_node_eligible{node_id="3",region="us-east"} 1
ggscale_relay_registry_node_eligible{node_id="4",region="us-west"} 0
# HELP ggscale_relay_registry_node_probe_ok 1 when the last probe of this registered relay node succeeded.
# TYPE ggscale_relay_registry_node_probe_ok gauge
ggscale_relay_registry_node_probe_ok{node_id="3",region="us-east"} 1
ggscale_relay_registry_node_probe_ok{node_id="4",region="us-west"} 0
`
	require.NoError(t, testutil.CollectAndCompare(c, strings.NewReader(expected)))
}

func TestCollector_should_skip_retired_nodes(t *testing.T) {
	c := relaynode.NewCollector(listOf(
		relaynode.Node{ID: 1, Region: "us-east", State: relaynode.StateRetired},
	))

	assert.Equal(t, 0, testutil.CollectAndCount(c))
}

func TestCollector_should_report_nothing_when_the_registry_cannot_be_read(t *testing.T) {
	c := relaynode.NewCollector(func(context.Context) ([]relaynode.Node, error) {
		return nil, errors.New("db down")
	})

	assert.Equal(t, 0, testutil.CollectAndCount(c))
}
