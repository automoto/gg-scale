package cacheinvalidation

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingNotifier struct {
	channel string
	payload string
}

func (n *recordingNotifier) Notify(_ context.Context, channel, payload string) error {
	n.channel = channel
	n.payload = payload
	return nil
}

func TestNotify_should_encode_an_api_key_event(t *testing.T) {
	// Arrange
	notifier := &recordingNotifier{}

	// Act
	err := Notify(context.Background(), notifier, Event{Kind: KindAPIKey, TenantID: 42})

	// Assert
	require.NoError(t, err)
	assert.Equal(t, Channel, notifier.channel)
	var event Event
	require.NoError(t, json.Unmarshal([]byte(notifier.payload), &event))
	assert.Equal(t, Event{Kind: KindAPIKey, TenantID: 42}, event)
}

func TestHandler_should_route_each_invalidation_event(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		install func(*int64, *int64, *int64, *int64) Handler
		assert  func(*testing.T, *int64, *int64, *int64, *int64)
	}{
		{
			name: "api key", payload: `{"kind":"api_key","tenant_id":7}`,
			install: func(key, _, _, _ *int64) Handler {
				return Handler{APIKeys: func(id int64) { *key = id }}
			},
			assert: func(t *testing.T, key, _, _, _ *int64) { assert.Equal(t, int64(7), *key) },
		},
		{
			name: "override", payload: `{"kind":"rate_limit_override","tenant_id":8}`,
			install: func(_, override, _, _ *int64) Handler {
				return Handler{Overrides: func(id int64) { *override = id }}
			},
			assert: func(t *testing.T, _, override, _, _ *int64) { assert.Equal(t, int64(8), *override) },
		},
		{
			name: "player epoch", payload: `{"kind":"player_epoch","player_id":9}`,
			install: func(_, _, player, _ *int64) Handler {
				return Handler{PlayerEpochs: func(id int64) { *player = id }}
			},
			assert: func(t *testing.T, _, _, player, _ *int64) { assert.Equal(t, int64(9), *player) },
		},
		{
			name: "tenant epochs", payload: `{"kind":"tenant_epochs","tenant_id":10}`,
			install: func(_, _, _, tenant *int64) Handler {
				return Handler{TenantEpochs: func(id int64) { *tenant = id }}
			},
			assert: func(t *testing.T, _, _, _, tenant *int64) { assert.Equal(t, int64(10), *tenant) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			var key, override, player, tenant int64
			handler := tt.install(&key, &override, &player, &tenant)

			// Act
			err := handler.Handle(tt.payload)

			// Assert
			require.NoError(t, err)
			tt.assert(t, &key, &override, &player, &tenant)
		})
	}
}

func TestHandler_should_reject_malformed_payload(t *testing.T) {
	// Arrange
	handler := Handler{}

	// Act
	err := handler.Handle(`{"kind"`)

	// Assert
	assert.Error(t, err)
}

func TestHandler_should_ignore_unknown_events_for_rolling_upgrades(t *testing.T) {
	// Arrange
	handler := Handler{}

	// Act
	err := handler.Handle(`{"kind":"added_in_a_later_release","tenant_id":7}`)

	// Assert
	assert.NoError(t, err)
}

type reconnectingListener struct {
	calls  atomic.Int64
	events chan struct{}
}

func (l *reconnectingListener) ListenChannel(ctx context.Context, _ string, fn func(string)) error {
	if l.calls.Add(1) < 3 {
		return errors.New("connection lost")
	}
	fn(`{"kind":"player_epoch","player_id":42}`)
	close(l.events)
	<-ctx.Done()
	return nil
}

func TestRun_should_reconnect_after_lost_connections(t *testing.T) {
	// Arrange
	listener := &reconnectingListener{events: make(chan struct{})}
	players := make(chan int64, 1)
	handler := Handler{PlayerEpochs: func(id int64) { players <- id }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		run(ctx, listener, handler, nil, time.Hour, time.Millisecond)
		close(done)
	}()

	// Act
	select {
	case <-listener.events:
	case <-time.After(time.Second):
		t.Fatal("listener did not reconnect")
	}
	cancel()

	// Assert
	select {
	case playerID := <-players:
		assert.Equal(t, int64(42), playerID)
	case <-time.After(time.Second):
		t.Fatal("notification was not handled")
	}
	<-done
	assert.Equal(t, int64(3), listener.calls.Load())
}
