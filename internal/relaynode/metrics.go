package relaynode

import (
	"context"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// collectTimeout bounds the registry read of one scrape.
const collectTimeout = 2 * time.Second

var (
	eligibleDesc = prometheus.NewDesc("ggscale_relay_registry_node_eligible",
		"1 when relay credentials advertise this registered relay node.", []string{"node_id", "region"}, nil)
	probeOKDesc = prometheus.NewDesc("ggscale_relay_registry_node_probe_ok",
		"1 when the last probe of this registered relay node succeeded.", []string{"node_id", "region"}, nil)
)

// Collector reports each known node's eligibility and last probe result at
// scrape time, so an alert can see a node that serves but fails its probe.
// Every API server reads the same registry; aggregate with max by (node_id).
type Collector struct {
	list func(context.Context) ([]Node, error)
}

// NewCollector returns a Collector over list, normally Store.List.
func NewCollector(list func(context.Context) ([]Node, error)) *Collector {
	return &Collector{list: list}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- eligibleDesc
	ch <- probeOKDesc
}

// Collect implements prometheus.Collector. A failed read reports no nodes
// rather than failing the whole scrape.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	nodes, err := c.list(ctx)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.State != StateKnown {
			continue
		}
		id := strconv.FormatInt(n.ID, 10)
		ch <- prometheus.MustNewConstMetric(eligibleDesc, prometheus.GaugeValue, gaugeBool(n.Eligible), id, n.Region)
		ch <- prometheus.MustNewConstMetric(probeOKDesc, prometheus.GaugeValue, gaugeBool(n.LastProbeOK), id, n.Region)
	}
}

func gaugeBool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
