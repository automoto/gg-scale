// Package lifecycle is the process lifecycle contract shared by the relay node
// and the API server: a starting → active ⇄ draining → stopping state machine,
// the /readyz and /lifecycle HTTP routes, their bearer guard, the lifecycle
// metrics, and the SIGTERM drain wait. Each process supplies its own meaning of
// "work" through Drainable; everything else is identical across processes.
package lifecycle

import (
	"context"
	"sync"
	"time"
)

// State is one step of the process lifecycle.
type State string

// The lifecycle states. A process starts in StateStarting, becomes
// StateActive once it can take traffic, moves between StateActive and
// StateDraining on operator request, and ends in StateStopping on SIGTERM.
const (
	StateStarting State = "starting"
	StateActive   State = "active"
	StateDraining State = "draining"
	StateStopping State = "stopping"
)

// States lists every state in lifecycle order.
var States = []State{StateStarting, StateActive, StateDraining, StateStopping}

// Drainable is a process's meaning of work.
type Drainable interface {
	// BeginDrain stops accepting new work.
	BeginDrain()
	// EndDrain accepts new work again.
	EndDrain()
	// ActiveWork reports what is still in flight.
	ActiveWork() int64
}

// RefusalCounter is implemented by a Drainable that counts the work it
// refused while draining. Gate implements it.
type RefusalCounter interface {
	RefusedWork() int64
}

// DefaultDrainTimeout bounds the SIGTERM drain wait when DRAIN_TIMEOUT is unset.
const DefaultDrainTimeout = 30 * time.Second

// defaultPollInterval is how often Shutdown samples ActiveWork.
const defaultPollInterval = 100 * time.Millisecond

// Config wires New.
type Config struct {
	// Role names the process in the /lifecycle JSON: "relay" or "api".
	Role string
	// Commit is the build commit reported in the /lifecycle JSON.
	Commit string
	// Token guards the /lifecycle routes. Empty leaves GET /lifecycle open and
	// leaves the POST routes unregistered.
	Token string
	// Work is the process's drainable work. Required.
	Work Drainable
	// Details returns the role-specific "details" object. nil reports {}.
	Details func() map[string]any
	// Now overrides the clock in tests. nil uses time.Now.
	Now func() time.Time
	// PollInterval overrides how often Shutdown samples ActiveWork. Zero uses
	// 100ms.
	PollInterval time.Duration
}

// Controller owns one process's lifecycle state. Safe for concurrent use.
type Controller struct {
	role         string
	commit       string
	token        string
	work         Drainable
	details      func() map[string]any
	now          func() time.Time
	pollInterval time.Duration
	startedAt    time.Time

	mu    sync.Mutex
	state State
	ready bool
	// shuttingDown is set when Shutdown starts, before its drain wait, so a
	// resume during the wait cannot put an exiting process back in service.
	shuttingDown   bool
	drainStartedAt time.Time
}

// New returns a Controller in StateStarting. The process calls MarkReady once
// it can take traffic.
func New(cfg Config) *Controller {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	return &Controller{
		role:         cfg.Role,
		commit:       cfg.Commit,
		token:        cfg.Token,
		work:         cfg.Work,
		details:      cfg.Details,
		now:          now,
		pollInterval: poll,
		startedAt:    now().UTC(),
		state:        StateStarting,
	}
}

// State reports the current state.
func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// MarkReady records that the process finished starting. A process still in
// StateStarting becomes StateActive; one drained during startup stays
// draining until resumed.
func (c *Controller) MarkReady() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = true
	if c.state == StateStarting {
		c.state = StateActive
	}
}

// Drain moves the process to StateDraining and stops new work. It is
// idempotent, and a no-op once the process is stopping.
func (c *Controller) Drain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.beginDrainLocked()
}

func (c *Controller) beginDrainLocked() {
	if c.state == StateDraining || c.state == StateStopping {
		return
	}
	c.state = StateDraining
	c.drainStartedAt = c.now().UTC()
	c.work.BeginDrain()
}

// Resume returns a draining process to StateActive, or to StateStarting when
// it never finished starting. It is idempotent. It returns false once
// Shutdown has started, because a process that is exiting cannot take work
// again.
func (c *Controller) Resume() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shuttingDown {
		return false
	}
	if c.state == StateDraining {
		c.work.EndDrain()
		c.drainStartedAt = time.Time{}
		c.state = StateStarting
		if c.ready {
			c.state = StateActive
		}
	}
	return true
}

// Shutdown is the SIGTERM path. It drains the process, waits until
// ActiveWork reaches zero or timeout elapses, then moves to StateStopping.
// It returns the work still in flight when the wait ended. The caller runs
// its existing shutdown path afterwards.
func (c *Controller) Shutdown(timeout time.Duration) int64 {
	c.mu.Lock()
	c.shuttingDown = true
	c.beginDrainLocked()
	c.mu.Unlock()
	remaining := c.waitForIdle(timeout)
	c.mu.Lock()
	c.state = StateStopping
	c.mu.Unlock()
	return remaining
}

func (c *Controller) waitForIdle(timeout time.Duration) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	for {
		remaining := c.work.ActiveWork()
		if remaining <= 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return remaining
		case <-ticker.C:
		}
	}
}

// Status is the /lifecycle JSON document. Every field but Details means the
// same thing for every process.
type Status struct {
	Role           string         `json:"role"`
	State          State          `json:"state"`
	Commit         string         `json:"commit"`
	StartedAt      time.Time      `json:"started_at"`
	DrainStartedAt *time.Time     `json:"drain_started_at"`
	ActiveWork     int64          `json:"active_work"`
	Details        map[string]any `json:"details"`
}

// Status snapshots the current lifecycle state.
func (c *Controller) Status() Status {
	c.mu.Lock()
	st := Status{
		Role:      c.role,
		State:     c.state,
		Commit:    c.commit,
		StartedAt: c.startedAt,
	}
	if !c.drainStartedAt.IsZero() {
		drainStartedAt := c.drainStartedAt
		st.DrainStartedAt = &drainStartedAt
	}
	c.mu.Unlock()

	st.ActiveWork = c.work.ActiveWork()
	st.Details = map[string]any{}
	if c.details != nil {
		if d := c.details(); d != nil {
			st.Details = d
		}
	}
	return st
}

func (c *Controller) refusedWork() int64 {
	rc, ok := c.work.(RefusalCounter)
	if !ok {
		return 0
	}
	return rc.RefusedWork()
}
