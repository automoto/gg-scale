package fleet_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/fleet"
)

const testTTL = 30 * time.Second

func sample(tenantID, projectID int64) fleet.RegisterParams {
	return fleet.RegisterParams{
		TenantID:   tenantID,
		ProjectID:  projectID,
		Name:       "doomerang-1",
		Address:    "localhost:7373",
		Version:    "0.1.0",
		Region:     "us-east-1",
		MaxPlayers: 4,
	}
}

func TestRegister_assigns_id_and_stores_server(t *testing.T) {
	r := fleet.NewRegistry(testTTL)

	id, err := r.Register(sample(1, 10))

	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, id)

	servers := r.List(1, 10)
	require.Len(t, servers, 1)
	assert.Equal(t, id, servers[0].ID)
	assert.Equal(t, "doomerang-1", servers[0].Name)
	assert.Equal(t, "localhost:7373", servers[0].Address)
}

func TestRegister_rejects_zero_tenant_or_project(t *testing.T) {
	r := fleet.NewRegistry(testTTL)

	cases := []struct {
		name   string
		params fleet.RegisterParams
	}{
		{"zero_tenant", fleet.RegisterParams{TenantID: 0, ProjectID: 10, Address: "h:1"}},
		{"zero_project", fleet.RegisterParams{TenantID: 1, ProjectID: 0, Address: "h:1"}},
		{"empty_address", fleet.RegisterParams{TenantID: 1, ProjectID: 10, Address: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.Register(c.params)
			assert.Error(t, err)
		})
	}
}

func TestHeartbeat_updates_last_heartbeat(t *testing.T) {
	r := fleet.NewRegistry(testTTL)
	id, err := r.Register(sample(1, 10))
	require.NoError(t, err)

	first := r.List(1, 10)[0].LastHeartbeat
	time.Sleep(2 * time.Millisecond)

	require.NoError(t, r.Heartbeat(1, id))

	second := r.List(1, 10)[0].LastHeartbeat
	assert.True(t, second.After(first), "heartbeat should advance LastHeartbeat")
}

func TestHeartbeat_unknown_id_returns_not_found(t *testing.T) {
	r := fleet.NewRegistry(testTTL)

	err := r.Heartbeat(1, uuid.New())

	assert.ErrorIs(t, err, fleet.ErrNotFound)
}

func TestHeartbeat_other_tenant_returns_not_found(t *testing.T) {
	r := fleet.NewRegistry(testTTL)
	id, err := r.Register(sample(1, 10))
	require.NoError(t, err)

	err = r.Heartbeat(2, id)

	assert.ErrorIs(t, err, fleet.ErrNotFound)
}

func TestList_only_returns_servers_in_same_project(t *testing.T) {
	r := fleet.NewRegistry(testTTL)
	_, err := r.Register(sample(1, 10))
	require.NoError(t, err)
	_, err = r.Register(sample(1, 11))
	require.NoError(t, err)
	_, err = r.Register(sample(2, 10))
	require.NoError(t, err)

	servers := r.List(1, 10)

	assert.Len(t, servers, 1)
}

func TestList_excludes_expired_servers(t *testing.T) {
	r := fleet.NewRegistry(10 * time.Millisecond)
	_, err := r.Register(sample(1, 10))
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond)

	assert.Empty(t, r.List(1, 10))
}

func TestSweep_removes_expired_entries(t *testing.T) {
	r := fleet.NewRegistry(10 * time.Millisecond)
	_, err := r.Register(sample(1, 10))
	require.NoError(t, err)

	time.Sleep(20 * time.Millisecond)
	r.Sweep()

	assert.Equal(t, 0, r.Size())
}

func TestDeregister_removes_server(t *testing.T) {
	r := fleet.NewRegistry(testTTL)
	id, err := r.Register(sample(1, 10))
	require.NoError(t, err)

	require.NoError(t, r.Deregister(1, id))

	assert.Empty(t, r.List(1, 10))
}

func TestDeregister_other_tenant_returns_not_found(t *testing.T) {
	r := fleet.NewRegistry(testTTL)
	id, err := r.Register(sample(1, 10))
	require.NoError(t, err)

	err = r.Deregister(2, id)

	assert.ErrorIs(t, err, fleet.ErrNotFound)
}
