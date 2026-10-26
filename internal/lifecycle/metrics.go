package lifecycle

import "github.com/prometheus/client_golang/prometheus"

var stateDesc = prometheus.NewDesc(
	"ggscale_lifecycle_state",
	"1 for the process's current lifecycle state, 0 otherwise.",
	[]string{"state"}, nil,
)

// Register exposes the lifecycle metrics on reg:
//
//   - ggscale_lifecycle_state{state="..."}: 1 for the current state, 0 for
//     the others;
//   - ggscale_lifecycle_work_refused_total: work refused while draining.
func (c *Controller) Register(reg prometheus.Registerer) error {
	if err := reg.Register(stateCollector{c}); err != nil {
		return err
	}
	return reg.Register(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "ggscale_lifecycle_work_refused_total",
		Help: "New work refused because the process was draining.",
	}, func() float64 { return float64(c.refusedWork()) }))
}

// stateCollector reads the state once per scrape, so every scrape reports
// exactly one state as 1 even while a transition is in progress.
type stateCollector struct{ c *Controller }

func (s stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- stateDesc }

func (s stateCollector) Collect(ch chan<- prometheus.Metric) {
	current := s.c.State()
	for _, state := range States {
		value := 0.0
		if state == current {
			value = 1
		}
		ch <- prometheus.MustNewConstMetric(stateDesc, prometheus.GaugeValue, value, string(state))
	}
}
