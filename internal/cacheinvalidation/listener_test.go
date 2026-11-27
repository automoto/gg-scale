package cacheinvalidation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeKeys struct {
	tenants []int64
	flushes int
}

func (f *fakeKeys) Invalidate(tenantID int64) { f.tenants = append(f.tenants, tenantID) }
func (f *fakeKeys) InvalidateAll()            { f.flushes++ }

type fakeOverrides struct {
	tenants []int64
	healthy []bool
}

func (f *fakeOverrides) Invalidate(tenantID int64) { f.tenants = append(f.tenants, tenantID) }
func (f *fakeOverrides) SetHealthy(healthy bool)   { f.healthy = append(f.healthy, healthy) }

type fakeEpochs struct {
	players []int64
	healthy []bool
}

func (f *fakeEpochs) Invalidate(playerID int64) { f.players = append(f.players, playerID) }
func (f *fakeEpochs) SetHealthy(healthy bool)   { f.healthy = append(f.healthy, healthy) }

func TestListener_should_route_each_cache_notification(t *testing.T) {
	keys := &fakeKeys{}
	overrides := &fakeOverrides{}
	epochs := &fakeEpochs{}
	listener := New(nil, keys, overrides, epochs, nil)

	listener.handle(`{"kind":"api_key","tenant_id":7}`)
	listener.handle(`{"kind":"override","tenant_id":8}`)
	listener.handle(`{"kind":"session_epoch","player_id":9}`)
	listener.handle(`{"kind":"unknown"}`)
	listener.handle(`not-json`)

	assert.Equal(t, []int64{7}, keys.tenants)
	assert.Equal(t, []int64{8}, overrides.tenants)
	assert.Equal(t, []int64{9}, epochs.players)
}

type fakeRelay struct {
	calls         int
	firstReady    chan struct{}
	secondStarted chan struct{}
	allowSecond   chan struct{}
}

func (r *fakeRelay) ListenChannelReady(ctx context.Context, _ string, ready func(), _ func(string)) error {
	r.calls++
	if r.calls == 1 {
		ready()
		close(r.firstReady)
		return errors.New("connection lost")
	}
	close(r.secondStarted)
	<-r.allowSecond
	ready()
	<-ctx.Done()
	return nil
}

func TestListener_should_mark_caches_down_before_reconnecting(t *testing.T) {
	keys := &fakeKeys{}
	overrides := &fakeOverrides{}
	epochs := &fakeEpochs{}
	relay := &fakeRelay{
		firstReady:    make(chan struct{}),
		secondStarted: make(chan struct{}),
		allowSecond:   make(chan struct{}),
	}
	listener := New(relay, keys, overrides, epochs, nil)
	listener.retryMin = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		listener.Run(ctx)
		close(done)
	}()

	<-relay.firstReady
	require.True(t, listener.Healthy())
	<-relay.secondStarted
	assert.False(t, listener.Healthy())
	assert.Equal(t, false, epochs.healthy[len(epochs.healthy)-1])
	assert.Equal(t, false, overrides.healthy[len(overrides.healthy)-1])
	close(relay.allowSecond)
	require.Eventually(t, listener.Healthy, time.Second, time.Millisecond)

	cancel()
	<-done
	assert.False(t, listener.Healthy())
	assert.Positive(t, keys.flushes)
}
