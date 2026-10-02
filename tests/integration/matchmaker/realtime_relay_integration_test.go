//go:build integration

// e2e:bucket b

package matchmaker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/realtime"
)

type chanWriter struct {
	once sync.Once
	got  chan []byte
}

func (w *chanWriter) Write(_ context.Context, data []byte) error {
	w.once.Do(func() { w.got <- append([]byte(nil), data...) })
	return nil
}

func (w *chanWriter) Close() error { return nil }

// Two hubs on one database act as two server hosts.
func TestRealtimeRelay_should_deliver_to_player_on_other_host(t *testing.T) {
	pool := db.NewPool(startMigratedDB(t))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sender, receiver := realtime.NewHub().WithRelay(pool), realtime.NewHub().WithRelay(pool)
	go receiver.RunRelay(ctx)
	w := &chanWriter{got: make(chan []byte, 1)}
	defer receiver.Register(7, 42, w)()
	msg := realtime.Message{Type: "party_changed", Payload: []byte(`{"party_id":1}`)}

	// The listener starts in the background, so push until it is ready.
	var got []byte
	require.Eventually(t, func() bool {
		require.NoError(t, sender.Push(ctx, 7, 42, msg))
		select {
		case got = <-w.got:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}, 10*time.Second, 10*time.Millisecond)

	assert.JSONEq(t, `{"type":"party_changed","payload":{"party_id":1}}`, string(got))
}
