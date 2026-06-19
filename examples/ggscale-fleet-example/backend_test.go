package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/fleet"
)

func TestNoopBackendNameIsExample(t *testing.T) {
	b := newNoopBackend()

	assert.Equal(t, "example", b.Name())
}

func TestNoopBackendAllocateReturnsReadyWithHardcodedAddress(t *testing.T) {
	b := newNoopBackend()

	got, err := b.Allocate(context.Background(), fleet.AllocationRequest{
		TenantID: 1, ProjectID: 2, Region: "us-east-1", GameMode: "ranked",
	})

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, fleet.StatusReady, got.Status)
	assert.NotEmpty(t, got.Address)
}

func TestNoopBackendAllocatePropagatesRequestFields(t *testing.T) {
	b := newNoopBackend()

	got, err := b.Allocate(context.Background(), fleet.AllocationRequest{
		TenantID: 42, ProjectID: 99, Region: "eu-1",
	})

	require.NoError(t, err)
	assert.Equal(t, int64(42), got.TenantID)
	assert.Equal(t, int64(99), got.ProjectID)
	assert.Equal(t, "eu-1", got.Region)
	assert.Equal(t, "example", got.Backend)
}

func TestNoopBackendAllocateAssignsUniqueBackendRef(t *testing.T) {
	b := newNoopBackend()

	a, err := b.Allocate(context.Background(), fleet.AllocationRequest{})
	require.NoError(t, err)
	b2, err := b.Allocate(context.Background(), fleet.AllocationRequest{})
	require.NoError(t, err)

	assert.NotEmpty(t, a.BackendRef)
	assert.NotEqual(t, a.BackendRef, b2.BackendRef)
}

func TestNoopBackendDeallocateReturnsNil(t *testing.T) {
	b := newNoopBackend()

	err := b.Deallocate(context.Background(), 1, "ref-1")

	assert.NoError(t, err)
}

func TestNoopBackendStatusIsReady(t *testing.T) {
	b := newNoopBackend()

	got, err := b.Status(context.Background(), 1, "ref-1")

	require.NoError(t, err)
	assert.Equal(t, fleet.StatusReady, got)
}

func TestNoopBackendWatchEmitsReadyThenCloses(t *testing.T) {
	b := newNoopBackend()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := b.Watch(ctx, 1, "ref-1")
	require.NoError(t, err)

	var updates []fleet.StatusUpdate
	timeout := time.After(2 * time.Second)
	for {
		select {
		case upd, ok := <-ch:
			if !ok {
				require.Len(t, updates, 1)
				assert.Equal(t, fleet.StatusReady, updates[0].Status)
				assert.NotEmpty(t, updates[0].Address)
				return
			}
			updates = append(updates, upd)
		case <-timeout:
			t.Fatalf("watch did not close in time; got %d updates", len(updates))
		}
	}
}

func TestNoopBackendHealthCheckIsNil(t *testing.T) {
	b := newNoopBackend()

	err := b.HealthCheck(context.Background())

	assert.NoError(t, err)
}
