package relaynode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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

// Probe failure reasons. They are stable strings, so a repeated failure can
// be recognised and the admin list can show why a node is unhealthy; the
// changing details, such as an error with an ephemeral port, go to the log.
const (
	FailureInvalidURL   = "invalid lifecycle_url"
	FailureBlocked      = "destination not allowed"
	FailureTimeout      = "timeout"
	FailureConnection   = "connection failed"
	FailureUnauthorized = "unauthorized"
	FailureInvalidJSON  = "invalid /lifecycle JSON"
	FailureUnknownState = "unknown lifecycle state"
	FailureNotRelay     = "role is not relay"
)

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
	// Failure is the stable reason OK is false: one of the Failure constants
	// or "status <code>".
	Failure string
	// Detail is the underlying error or value, for the log only.
	Detail string
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
// token is set and gives up on a node after timeout. Every connection passes
// the destination policy in checkProbeTarget.
func NewProber(token string, timeout time.Duration) *Prober {
	dialer := &net.Dialer{Timeout: timeout, Control: controlDial}
	return &Prober{
		token: token,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				// No proxy: the policy must see the node's own address.
				Proxy:               nil,
				DialContext:         dialer.DialContext,
				TLSHandshakeTimeout: timeout,
				MaxIdleConnsPerHost: 1,
				IdleConnTimeout:     2 * ProbeInterval,
			},
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
		return Result{Failure: FailureInvalidURL, Detail: err.Error()}
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Result{Failure: transportFailure(err), Detail: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return Result{Unauthorized: true, Failure: FailureUnauthorized}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{Failure: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	var st status
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxStatusBytes)).Decode(&st); err != nil {
		return Result{Failure: FailureInvalidJSON, Detail: err.Error()}
	}
	if !slices.Contains(lifecycle.States, st.State) {
		return Result{Failure: FailureUnknownState, Detail: fmt.Sprintf("state %q", st.State)}
	}
	r := Result{State: string(st.State), ActiveWork: st.ActiveWork}
	if st.Role != relayRole {
		r.Failure = FailureNotRelay
		r.Detail = fmt.Sprintf("role %q", st.Role)
		return r
	}
	r.OK = true
	return r
}

func transportFailure(err error) string {
	if errors.Is(err, errBlockedDestination) {
		return FailureBlocked
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return FailureTimeout
	}
	return FailureConnection
}

// ProbeStore is the part of Store the Monitor needs.
type ProbeStore interface {
	Known(ctx context.Context) ([]Node, error)
	RecordProbe(ctx context.Context, id int64, r Result) (prior PriorProbe, recorded bool, err error)
}

// Monitor probes every known node and records the results. It logs a
// failure when the node's stored last_probe_error changes, and a recovery
// when a failing node succeeds. The comparison uses the result each write
// replaced, so the log follows the database rather than one process's
// memory: any API server can run the probe.
type Monitor struct {
	store  ProbeStore
	prober *Prober
	logger *slog.Logger
}

// NewMonitor returns a Monitor.
func NewMonitor(store ProbeStore, prober *Prober, logger *slog.Logger) *Monitor {
	return &Monitor{store: store, prober: prober, logger: logger}
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
			prior, recorded, err := m.store.RecordProbe(ctx, n.ID, r)
			if err != nil {
				errs[i] = fmt.Errorf("record probe for relay node %d: %w", n.ID, err)
				return
			}
			if recorded {
				m.logTransition(ctx, n, prior, r)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// Run probes every interval until ctx ends, starting at once. It is the
// fallback when River, which normally schedules the probe, is unavailable.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := m.ProbeAll(ctx); err != nil && ctx.Err() == nil {
			m.logger.WarnContext(ctx, "relay node probe", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// logTransition compares r with prior, the result it replaced in the store.
// The store hands each prior to exactly one writer, so a change is logged
// once even when several API servers probe the same node.
func (m *Monitor) logTransition(ctx context.Context, n Node, prior PriorProbe, r Result) {
	failing := prior.At != nil && !prior.OK
	if r.OK {
		if failing {
			m.logger.InfoContext(ctx, "relay node probe recovered", "node_id", n.ID, "lifecycle_url", n.LifecycleURL)
		}
		return
	}
	if failing && prior.Error != nil && *prior.Error == r.Failure {
		return
	}
	if r.Unauthorized {
		m.logger.WarnContext(ctx, "relay node probe unauthorized: RELAY_PROBE_TOKEN does not match the node's LIFECYCLE_TOKEN",
			"node_id", n.ID, "lifecycle_url", n.LifecycleURL)
		return
	}
	m.logger.WarnContext(ctx, "relay node probe failed",
		"node_id", n.ID, "lifecycle_url", n.LifecycleURL, "reason", r.Failure, "detail", r.Detail)
}
