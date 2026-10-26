package lifecycle

import "github.com/prometheus/client_golang/prometheus"

// Register exposes the lifecycle metrics on reg:
//
//   - ggscale_lifecycle_state{state="..."}: 1 for the current state, 0 for
//     the others;
//   - ggscale_lifecycle_work_refused_total: work refused while draining.
func (c *Controller) Register(reg prometheus.Registerer) error {
	for _, s := range States {
		state := s
		gauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "ggscale_lifecycle_state",
			Help:        "1 for the process's current lifecycle state, 0 otherwise.",
			ConstLabels: prometheus.Labels{"state": string(state)},
		}, func() float64 {
			if c.State() == state {
				return 1
			}
			return 0
		})
		if err := reg.Register(gauge); err != nil {
			return err
		}
	}
	return reg.Register(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "ggscale_lifecycle_work_refused_total",
		Help: "New work refused because the process was draining.",
	}, func() float64 { return float64(c.refusedWork()) }))
}
