package jobs

import (
	"context"

	"github.com/riverqueue/river"
)

// RelayNodeProbeKind is the River job kind that probes registered relay
// nodes.
const RelayNodeProbeKind = "relay_node_probe"

// RelayNodeProbeArgs is the argument-less periodic probe job.
type RelayNodeProbeArgs struct{}

// Kind implements river.JobArgs.
func (RelayNodeProbeArgs) Kind() string { return RelayNodeProbeKind }

// InsertOpts makes a failed run final: the next periodic run probes again
// within seconds, so a retry would only queue stale work behind it.
func (RelayNodeProbeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1}
}

// RelayNodeProber probes every known relay node and records the results.
// relaynode.Monitor implements it.
type RelayNodeProber interface {
	ProbeAll(ctx context.Context) error
}

// RelayNodeProbeWorker runs the relay node probe. It never removes rows.
type RelayNodeProbeWorker struct {
	river.WorkerDefaults[RelayNodeProbeArgs]
	prober RelayNodeProber
}

// NewRelayNodeProbeWorker returns a worker that probes through prober.
func NewRelayNodeProbeWorker(prober RelayNodeProber) *RelayNodeProbeWorker {
	return &RelayNodeProbeWorker{prober: prober}
}

// Work implements river.Worker.
func (w *RelayNodeProbeWorker) Work(ctx context.Context, _ *river.Job[RelayNodeProbeArgs]) error {
	return w.prober.ProbeAll(ctx)
}
