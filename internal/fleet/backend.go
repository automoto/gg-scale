// Package fleet allocates and tracks game-server instances on behalf of the
// matchmaker. Backend is the contract every implementation (docker, agones,
// openstack, plugin) implements; Manager owns persistence, retry, and the
// single point of entry callers use.
package fleet

import (
	"context"
	"errors"
)

// Status reports the lifecycle position of an Allocation. The state machine
// is monotonic except for the terminal states (Shutdown, Failed) which are
// absorbing.
type Status int

// Allocation lifecycle states. The state machine is monotonic from
// StatusPending through StatusAllocated; StatusShutdown and StatusFailed are
// terminal.
const (
	StatusPending Status = iota
	StatusAllocating
	StatusReady
	StatusAllocated
	StatusDraining
	StatusShutdown
	StatusFailed
)

// String renders the status as a lowercase token matching the
// allocation_status Postgres enum.
func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusAllocating:
		return "allocating"
	case StatusReady:
		return "ready"
	case StatusAllocated:
		return "allocated"
	case StatusDraining:
		return "draining"
	case StatusShutdown:
		return "shutdown"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ParseStatus is the inverse of Status.String. Returns StatusFailed and a
// non-nil error for unknown tokens so callers cannot silently smuggle
// invalid states out of the database.
func ParseStatus(s string) (Status, error) {
	switch s {
	case "pending":
		return StatusPending, nil
	case "allocating":
		return StatusAllocating, nil
	case "ready":
		return StatusReady, nil
	case "allocated":
		return StatusAllocated, nil
	case "draining":
		return StatusDraining, nil
	case "shutdown":
		return StatusShutdown, nil
	case "failed":
		return StatusFailed, nil
	default:
		return StatusFailed, errors.New("fleet: unknown status " + s)
	}
}

// IsTerminal reports whether the status represents a closed allocation. The
// manager refuses to issue Watch/Deallocate calls past a terminal state.
func (s Status) IsTerminal() bool {
	return s == StatusShutdown || s == StatusFailed
}

// AllocationID is the manager's primary key for an allocation. Backends
// receive it in requests so they can correlate StatusUpdate stream entries.
type AllocationID int64

// AllocationRequest is the input a matchmaker hands to Manager.Allocate. The
// manager forwards it to the configured Backend after persisting a pending
// row.
//
// Trust boundary: Region, GameMode, and Labels are flow-through fields that
// land in backend-specific places (Kubernetes label selectors, Docker
// labels, plugin gRPC). They are treated here as already-validated inputs.
// The validation boundary lives at the matchmaker HTTP handler (M6), which
// is the surface receiving player SDK input — validating again here
// would put the check on the wrong side of the boundary.
type AllocationRequest struct {
	TenantID  int64
	ProjectID int64
	// FleetID identifies the fleet template this allocation is drawn from.
	// Required at the matchmaker / dashboard boundary; the Manager refuses
	// to dispatch a request with FleetID == 0.
	FleetID int64
	// Backend is the backend name resolved from the fleet template. The
	// Manager populates this from the fleet row before calling Backend.
	// Allocate, then refuses to dispatch when it disagrees with the
	// configured backend's Name().
	Backend string
	// Config is the per-backend recipe flattened to strings. Manager copies
	// fleet.Config into this field; backends read keys they care about
	// (docker: image/port/probe_*; agones: namespace/fleet_name/selector.*;
	// plugin: whatever the plugin defines).
	Config   map[string]string
	Region   string
	GameMode string
	Capacity int
	Labels   map[string]string
}

// Allocation is the manager's view of one game-server slot. BackendRef is
// the backend-specific identifier (Docker container ID, Agones GameServer
// name, OpenStack instance UUID, plugin-supplied opaque string).
type Allocation struct {
	ID         AllocationID
	TenantID   int64
	ProjectID  int64
	FleetID    int64
	Backend    string
	BackendRef string
	Region     string
	Address    string
	// Protocol is the wire protocol the game-server listens on,
	// lower-cased ("tcp", "udp", "tcpudp"). Surfaced to clients via the
	// matchmaker response's protocol_hint so cross-game launchers /
	// dashboards can dial the right transport. Empty when the backend
	// can't determine it (older backends, plugin backends that don't set
	// it). The client is built for a specific game and ultimately knows
	// its protocol; protocol_hint is defense-in-depth and observability.
	Protocol string
	Status   Status
	Metadata map[string]string
}

// StatusUpdate is the unit of progress a Backend.Watch pushes back to the
// manager. Address populates once the backend reaches StatusReady; Err is
// non-nil only on terminal failure.
type StatusUpdate struct {
	Status  Status
	Address string
	Err     error
}

// Backend allocates and tears down game-server slots. Each implementation
// (docker, agones, openstack, plugin) satisfies this contract and is
// otherwise opaque to the manager.
//
// Backends must be safe for concurrent use by the manager. Watch returns a
// channel the backend closes when it has no further updates to send or when
// ctx is cancelled — the manager treats a closed channel as a clean end of
// stream, not a failure.
type Backend interface {
	Name() string
	Allocate(ctx context.Context, req AllocationRequest) (*Allocation, error)
	Deallocate(ctx context.Context, id AllocationID, backendRef string) error
	Status(ctx context.Context, id AllocationID, backendRef string) (Status, error)
	Watch(ctx context.Context, id AllocationID, backendRef string) (<-chan StatusUpdate, error)
	HealthCheck(ctx context.Context) error
}

// ErrNotFound is returned by Backend implementations when the manager
// references an allocation the backend no longer knows about. The manager
// translates this into a 404 for HTTP callers and a re-queue for the
// matchmaker.
var ErrNotFound = errors.New("fleet: allocation not found")

// ErrUnsupported is returned by a Backend when asked to perform an operation
// it does not implement (e.g. Watch on a backend that only supports polling).
var ErrUnsupported = errors.New("fleet: operation not supported by backend")
