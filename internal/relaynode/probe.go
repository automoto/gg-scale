package relaynode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/automoto/gg-scale/internal/lifecycle"
)

// DefaultProbeTimeout bounds one GET /lifecycle. It stays well under
// ProbeInterval so a slow node cannot stack probes.
const DefaultProbeTimeout = 3 * time.Second

// maxStatusBytes caps how much of a /lifecycle response the prober reads.
const maxStatusBytes = 65536

// relayRole is the role a relay node reports in its /lifecycle JSON.
const relayRole = "relay"

// Result is one probe of a node's GET /lifecycle.
type Result struct {
	// OK is true when the node answered 2xx with a valid relay status. A
	// draining node is OK; eligibility also needs State "active".
	OK bool
	// State is the lifecycle state read, or "" when none could be read.
	State string
	// ActiveWork is the active_work read, or nil when none could be read.
	ActiveWork *int64
	// Unauthorized is true when the node answered 401: RELAY_PROBE_TOKEN does
	// not match the node's LIFECYCLE_TOKEN.
	Unauthorized bool
	// Failure says why OK is false.
	Failure string
}

// status is the shared top-level part of the /lifecycle JSON. The prober
// never reads details, so it works for any process that implements the
// lifecycle contract.
type status struct {
	Role       string          `json:"role"`
	State      lifecycle.State `json:"state"`
	ActiveWork *int64          `json:"active_work"`
}

// Prober reads a node's GET /lifecycle.
type Prober struct {
	client *http.Client
	token  string
}

// NewProber returns a Prober that sends "Authorization: Bearer <token>" when
// token is set and gives up on a node after timeout.
func NewProber(token string, timeout time.Duration) *Prober {
	return &Prober{
		token: token,
		client: &http.Client{
			Timeout: timeout,
			// A redirect is a misconfigured lifecycle_url, not a healthy node.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Probe reads one node. It never returns an error: every failure is a Result
// with OK false.
func (p *Prober) Probe(ctx context.Context, lifecycleURL string) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lifecycleURL, nil)
	if err != nil {
		return Result{Failure: err.Error()}
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{Failure: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return Result{Unauthorized: true, Failure: "401 unauthorized"}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{Failure: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	var st status
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatusBytes)).Decode(&st); err != nil {
		return Result{Failure: "decode /lifecycle: " + err.Error()}
	}
	if !slices.Contains(lifecycle.States, st.State) {
		return Result{Failure: fmt.Sprintf("unknown lifecycle state %q", st.State)}
	}
	r := Result{State: string(st.State), ActiveWork: st.ActiveWork}
	if st.Role != relayRole {
		r.Failure = fmt.Sprintf("role %q is not %q", st.Role, relayRole)
		return r
	}
	r.OK = true
	return r
}

// ProbeStore is the part of Store the Monitor needs.
type ProbeStore interface {
	Known(ctx context.Context) ([]Node, error)
	RecordProbe(ctx context.Context, id int64, r Result) error
}

// Monitor probes every known node and records the results. It logs a node's
// failure when it starts or changes, not on every probe. Safe for concurrent
// use, though the probe job runs it on one worker at a time.
type Monitor struct {
	store  ProbeStore
	prober *Prober
	logger *slog.Logger

	mu sync.Mutex
	// failures holds each failing node's last logged failure.
	failures map[int64]string
}

// NewMonitor returns a Monitor.
func NewMonitor(store ProbeStore, prober *Prober, logger *slog.Logger) *Monitor {
	return &Monitor{store: store, prober: prober, logger: logger, failures: map[int64]string{}}
}

// ProbeAll probes every known node concurrently and records each result. It
// returns the first store error; probe failures are recorded, not returned.
func (m *Monitor) ProbeAll(ctx context.Context) error {
	nodes, err := m.store.Known(ctx)
	if err != nil {
		return fmt.Errorf("list known relay nodes: %w", err)
	}
	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Go(func() {
			r := m.prober.Probe(ctx, n.LifecycleURL)
			m.logTransition(ctx, n, r)
			if err := m.store.RecordProbe(ctx, n.ID, r); err != nil {
				errs[i] = fmt.Errorf("record probe for relay node %d: %w", n.ID, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (m *Monitor) logTransition(ctx context.Context, n Node, r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, failing := m.failures[n.ID]
	if r.OK {
		if failing {
			delete(m.failures, n.ID)
			m.logger.InfoContext(ctx, "relay node probe recovered", "node_id", n.ID, "lifecycle_url", n.LifecycleURL)
		}
		return
	}
	if failing && prev == r.Failure {
		return
	}
	m.failures[n.ID] = r.Failure
	if r.Unauthorized {
		m.logger.WarnContext(ctx, "relay node probe unauthorized: RELAY_PROBE_TOKEN does not match the node's LIFECYCLE_TOKEN",
			"node_id", n.ID, "lifecycle_url", n.LifecycleURL)
		return
	}
	m.logger.WarnContext(ctx, "relay node probe failed", "node_id", n.ID, "lifecycle_url", n.LifecycleURL, "reason", r.Failure)
}
