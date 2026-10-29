package observability

import "github.com/prometheus/client_golang/prometheus"

// RegisterBuildInfo exposes ggscale_build_info, so dashboards can show which
// release each process runs.
func RegisterBuildInfo(reg prometheus.Registerer, version, commit string) {
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "ggscale_build_info",
		Help:        "Always 1. The labels name the release and commit this process runs.",
		ConstLabels: prometheus.Labels{"version": version, "commit": commit},
	})
	info.Set(1)
	reg.MustRegister(info)
}
