package jobs

import (
	"context"
	"time"

	"github.com/riverqueue/river"

	"github.com/automoto/gg-scale/internal/relaynode"
)

// RelayNodeProbeKind is the River job kind that probes registered relay
// nodes.
const RelayNodeProbeKind = "relay_node_probe"

// RelayNodeProbeQueue is the probe's own River queue. Relay credentials stop
// listing every node once probes fall 30s behind, so the probe must not wait
// behind slow jobs on the shared default queue.
const RelayNodeProbeQueue = "relay_node_probe"

// RelayNodeProbeArgs is the argument-less periodic probe job.
type RelayNodeProbeArgs struct{}

// Kind implements river.JobArgs.
func (RelayNodeProbeArgs) Kind() string { return RelayNodeProbeKind }

// InsertOpts puts the probe on its own queue and makes a failed run final:
// the next periodic run probes again within seconds, so a retry would only
// queue stale work behind it.
func (RelayNodeProbeArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, Queue: RelayNodeProbeQueue}
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

// Timeout bounds one run to a probe interval, so a stuck run cannot hold the
// probe queue's only worker past the next run.
func (w *RelayNodeProbeWorker) Timeout(*river.Job[RelayNodeProbeArgs]) time.Duration {
	return relaynode.ProbeInterval
}

// Work implements river.Worker.
func (w *RelayNodeProbeWorker) Work(ctx context.Context, _ *river.Job[RelayNodeProbeArgs]) error {
	return w.prober.ProbeAll(ctx)
}
