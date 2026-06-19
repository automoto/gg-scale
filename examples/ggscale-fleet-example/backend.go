package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/ggscale/ggscale/internal/fleet"
)

// noopBackend is the reference fleet.Backend implementation. It allocates
// nothing — every call resolves instantly to a hard-coded address — so it
// doubles as the third-party plugin author template and as the M4.4
// integration-test fixture.
type noopBackend struct {
	counter atomic.Uint64
}

func newNoopBackend() *noopBackend { return &noopBackend{} }

const (
	noopBackendName    = "example"
	noopBackendAddress = "127.0.0.1:7777"
)

func (b *noopBackend) Name() string { return noopBackendName }

func (b *noopBackend) Allocate(_ context.Context, req fleet.AllocationRequest) (*fleet.Allocation, error) {
	ref := fmt.Sprintf("noop-%d", b.counter.Add(1))
	return &fleet.Allocation{
		TenantID:   req.TenantID,
		ProjectID:  req.ProjectID,
		Backend:    noopBackendName,
		BackendRef: ref,
		Region:     req.Region,
		Address:    noopBackendAddress,
		Status:     fleet.StatusReady,
	}, nil
}

func (b *noopBackend) Deallocate(_ context.Context, _ fleet.AllocationID, _ string) error {
	return nil
}

func (b *noopBackend) Status(_ context.Context, _ fleet.AllocationID, _ string) (fleet.Status, error) {
	return fleet.StatusReady, nil
}

func (b *noopBackend) Watch(_ context.Context, _ fleet.AllocationID, _ string) (<-chan fleet.StatusUpdate, error) {
	ch := make(chan fleet.StatusUpdate, 1)
	ch <- fleet.StatusUpdate{Status: fleet.StatusReady, Address: noopBackendAddress}
	close(ch)
	return ch, nil
}

func (b *noopBackend) HealthCheck(_ context.Context) error { return nil }

// Ping satisfies the optional pinger contract used by the host's supervisor.
// GGSCALE_EXAMPLE_FAIL_PING=1 makes every probe fail — used by the
// supervisor's exhaust-budget integration test.
func (b *noopBackend) Ping(_ context.Context) error {
	if os.Getenv("GGSCALE_EXAMPLE_FAIL_PING") == "1" {
		return errors.New("example: ping intentionally failing")
	}
	return nil
}
