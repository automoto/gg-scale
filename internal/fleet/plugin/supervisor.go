package plugin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ggscale/ggscale/internal/fleet"
)

// SupervisorConfig wires NewSupervisor. The interval/budget knobs exist
// primarily for tests; production uses the defaults.
type SupervisorConfig struct {
	Launch       LaunchConfig
	MaxRestarts  int           // default 3
	PollInterval time.Duration // default 1s

	// PingInterval catches hung-but-alive plugins that Exited() can't see.
	PingInterval      time.Duration // default 10s
	PingFailureBudget int           // default 3
}

// Supervisor wraps Plugin lifecycle: detects subprocess exit, restarts up to
// MaxRestarts consecutive deaths, and forwards fleet.Backend calls to
// whichever plugin is currently live. Implements fleet.Backend + io.Closer.
//
// restartCount is "consecutive failures since last healthy probe" — a
// successful Ping (in pingLoop or the immediate post-restart probe) resets
// it. Without that reset, a plugin that crashes once a year would
// eventually be abandoned.
type Supervisor struct {
	cfg SupervisorConfig

	mu      sync.RWMutex
	current *Plugin
	closed  bool // Close() was invoked; swap must refuse to adopt new plugins

	restartCount  atomic.Int64
	totalRestarts atomic.Int64
	done          chan struct{}
	doneOnce      sync.Once

	wg sync.WaitGroup // joined by Close()
}

// NewSupervisor launches the plugin once and starts the watcher + pingLoop
// goroutines. Returns an error if the initial Launch fails.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.MaxRestarts == 0 {
		cfg.MaxRestarts = 3
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 10 * time.Second
	}
	if cfg.PingFailureBudget == 0 {
		cfg.PingFailureBudget = 3
	}
	p, err := Launch(cfg.Launch)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{cfg: cfg, current: p, done: make(chan struct{})}
	s.wg.Add(2)
	go s.watch()
	go s.pingLoop()
	return s, nil
}

func (s *Supervisor) shutdown() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *Supervisor) watch() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.mu.RLock()
			p := s.current
			s.mu.RUnlock()
			if p == nil || !p.client.Exited() {
				continue
			}
			s.handleExit()
		}
	}
}

// pingLoop probes the live plugin's gRPC Ping. After PingFailureBudget
// consecutive failures it force-kills the subprocess; the watch loop then
// observes the death and runs the normal restart path. Each healthy probe
// also resets the global restartCount so a once-in-a-blue-moon crash
// doesn't accumulate into permanent abandonment.
func (s *Supervisor) pingLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.PingInterval)
	defer t.Stop()
	consecutive := 0
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			// Reading current under RLock then dropping it leaves a small
			// window where swap can occur before Ping is sent. The error
			// path (incl. force-kill) re-checks current==p, so a stale
			// Ping is at worst accounted to the wrong consecutive counter
			// for one cycle.
			s.mu.RLock()
			p := s.current
			s.mu.RUnlock()
			if p == nil {
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.PingInterval/2)
			err := p.Ping(ctx)
			cancel()

			if err == nil {
				consecutive = 0
				s.restartCount.Store(0)
				continue
			}
			consecutive++
			if consecutive < s.cfg.PingFailureBudget {
				continue
			}
			// Force the subprocess down — but only if it's still the
			// current plugin. A concurrent swap from handleExit could have
			// already replaced it; killing the new one would be incorrect.
			s.mu.RLock()
			stillCurrent := s.current == p
			s.mu.RUnlock()
			if stillCurrent {
				_ = p.Close()
			}
			consecutive = 0
		}
	}
}

func (s *Supervisor) handleExit() {
	if int(s.restartCount.Load()) >= s.cfg.MaxRestarts {
		s.swap(nil)
		s.shutdown() // permanent give-up; stop both goroutines
		return
	}
	s.restartCount.Add(1)
	s.totalRestarts.Add(1)
	p, err := Launch(s.cfg.Launch)
	if err != nil {
		// Slot already consumed; the next tick retries until exhaustion.
		s.swap(nil)
		return
	}
	s.swap(p)

	// Immediate liveness probe resets the budget if the new plugin is
	// healthy — covers fast crash-loops where pingLoop's cadence is slower
	// than the crashes.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.PingInterval/2)
	if perr := p.Ping(ctx); perr == nil {
		s.restartCount.Store(0)
	}
	cancel()
}

// swap replaces s.current with p and closes the previous plugin. If the
// supervisor is shutting down, the incoming p is closed instead of being
// adopted, so a Launch that completes after Close() does not leak the
// subprocess.
func (s *Supervisor) swap(p *Plugin) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		if p != nil {
			_ = p.Close()
		}
		return
	}
	old := s.current
	s.current = p
	s.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// Pid returns the current plugin subprocess PID, or 0 if no plugin is live.
func (s *Supervisor) Pid() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return 0
	}
	return s.current.Pid()
}

// Manifest returns the parsed manifest from the live plugin, or nil if no
// plugin is live or no sidecar was found.
func (s *Supervisor) Manifest() *Manifest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return nil
	}
	return s.current.Manifest()
}

// RestartCount is the number of consecutive failed lifecycles since the
// last healthy Ping; reset on every successful probe.
func (s *Supervisor) RestartCount() int {
	return int(s.restartCount.Load())
}

// TotalRestartCount is the lifetime count of subprocess restarts. Never
// resets — useful for ops dashboards and tests that assert a restart
// happened without racing the consecutive-count reset.
func (s *Supervisor) TotalRestartCount() int {
	return int(s.totalRestarts.Load())
}

// Close stops the watcher + pingLoop and kills the current subprocess. Safe
// to call concurrently with an in-flight restart: swap() rejects the new
// plugin and closes it instead.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	s.closed = true
	old := s.current
	s.current = nil
	s.mu.Unlock()

	s.shutdown()
	s.wg.Wait()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

var errNoPlugin = errors.New("fleet plugin: supervisor has no live plugin")

func (s *Supervisor) backend() (fleet.Backend, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return nil, errNoPlugin
	}
	return s.current.Backend, nil
}

// Name forwards to the live plugin, or returns "plugin" if none.
func (s *Supervisor) Name() string {
	b, err := s.backend()
	if err != nil {
		return "plugin"
	}
	return b.Name()
}

// Allocate forwards to the live plugin.
func (s *Supervisor) Allocate(ctx context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	b, err := s.backend()
	if err != nil {
		return nil, err
	}
	return b.Allocate(ctx, req)
}

// Deallocate forwards to the live plugin.
func (s *Supervisor) Deallocate(ctx context.Context, id fleet.AllocationID, ref string) error {
	b, err := s.backend()
	if err != nil {
		return err
	}
	return b.Deallocate(ctx, id, ref)
}

// Status forwards to the live plugin.
func (s *Supervisor) Status(ctx context.Context, id fleet.AllocationID, ref string) (fleet.Status, error) {
	b, err := s.backend()
	if err != nil {
		return fleet.StatusFailed, err
	}
	return b.Status(ctx, id, ref)
}

// Watch forwards to the live plugin.
func (s *Supervisor) Watch(ctx context.Context, id fleet.AllocationID, ref string) (<-chan fleet.StatusUpdate, error) {
	b, err := s.backend()
	if err != nil {
		return nil, err
	}
	return b.Watch(ctx, id, ref)
}

// HealthCheck forwards to the live plugin.
func (s *Supervisor) HealthCheck(ctx context.Context) error {
	b, err := s.backend()
	if err != nil {
		return err
	}
	return b.HealthCheck(ctx)
}
