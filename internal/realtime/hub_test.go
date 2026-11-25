package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/realtime"
)

type fakeWriter struct {
	mu     sync.Mutex
	writes [][]byte
	closed atomic.Bool
	err    error
}

func (f *fakeWriter) Write(_ context.Context, data []byte) error {
	if f.closed.Load() {
		return errors.New("closed")
	}
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	dup := make([]byte, len(data))
	copy(dup, data)
	f.writes = append(f.writes, dup)
	return nil
}

func (f *fakeWriter) Close() error {
	f.closed.Store(true)
	return nil
}

func (f *fakeWriter) Writes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.writes))
	copy(out, f.writes)
	return out
}

func TestHubSendDeliversJSONToRegisteredWriter(t *testing.T) {
	h := realtime.NewHub()
	w := &fakeWriter{}
	unregister := h.Register(1, 42, w)
	defer unregister()

	err := h.Send(context.Background(), 1, 42, realtime.Message{Type: "match_ready"})

	require.NoError(t, err)
	writes := w.Writes()
	require.Len(t, writes, 1)
	var got realtime.Message
	require.NoError(t, json.Unmarshal(writes[0], &got))
	assert.Equal(t, "match_ready", got.Type)
}

func TestHubSendReturnsErrNotConnectedWhenAbsent(t *testing.T) {
	h := realtime.NewHub()

	err := h.Send(context.Background(), 1, 42, realtime.Message{Type: "x"})

	assert.ErrorIs(t, err, realtime.ErrNotConnected)
}

func TestHubRegisterReplacesPriorWriterAndClosesIt(t *testing.T) {
	h := realtime.NewHub()
	first := &fakeWriter{}
	second := &fakeWriter{}

	h.Register(1, 42, first)
	h.Register(1, 42, second)

	assert.True(t, first.closed.Load(), "old writer must be closed when replaced")
	require.NoError(t, h.Send(context.Background(), 1, 42, realtime.Message{Type: "x"}))
	assert.Empty(t, first.Writes())
	assert.Len(t, second.Writes(), 1)
}

func TestHubUnregisterRemovesMapping(t *testing.T) {
	h := realtime.NewHub()
	w := &fakeWriter{}
	unregister := h.Register(1, 42, w)

	unregister()

	err := h.Send(context.Background(), 1, 42, realtime.Message{Type: "x"})
	assert.ErrorIs(t, err, realtime.ErrNotConnected)
}

func TestHubUnregisterIsScopedToTheRegistrationItIssued(t *testing.T) {
	h := realtime.NewHub()
	first := &fakeWriter{}
	second := &fakeWriter{}

	unregisterFirst := h.Register(1, 42, first)
	h.Register(1, 42, second) // closes first; rebinds slot to second
	unregisterFirst()         // must NOT remove second from the slot

	require.NoError(t, h.Send(context.Background(), 1, 42, realtime.Message{Type: "x"}))
	assert.Len(t, second.Writes(), 1)
}

func TestHubIsolatesDifferentTenantsAndUsers(t *testing.T) {
	h := realtime.NewHub()
	tenantA := &fakeWriter{}
	tenantB := &fakeWriter{}
	userTwo := &fakeWriter{}
	h.Register(1, 42, tenantA)
	h.Register(2, 42, tenantB)
	h.Register(1, 43, userTwo)

	require.NoError(t, h.Send(context.Background(), 1, 42, realtime.Message{Type: "for-A-42"}))

	assert.Len(t, tenantA.Writes(), 1)
	assert.Empty(t, tenantB.Writes())
	assert.Empty(t, userTwo.Writes())
}

func TestHubConcurrentRegisterAndSendIsSafe(t *testing.T) {
	h := realtime.NewHub()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(id int64) {
			defer wg.Done()
			h.Register(1, id, &fakeWriter{})
		}(int64(i))
		go func(id int64) {
			defer wg.Done()
			_ = h.Send(context.Background(), 1, id, realtime.Message{Type: "x"})
		}(int64(i))
	}
	wg.Wait()
}

// slowCloseWriter blocks in Close until release is closed, like a socket
// whose close handshake waits on a stuck peer.
type slowCloseWriter struct {
	fakeWriter
	entered chan struct{}
	release chan struct{}
}

func (s *slowCloseWriter) Close() error {
	close(s.entered)
	<-s.release
	return s.fakeWriter.Close()
}

func TestHubRegister_should_not_block_other_players_while_the_old_socket_closes(t *testing.T) {
	h := realtime.NewHub()
	old := &slowCloseWriter{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(old.release) })
	h.Register(1, 42, old)
	h.Register(1, 7, &fakeWriter{})
	go h.Register(1, 42, &fakeWriter{})
	<-old.entered

	sendDone := make(chan error, 1)
	go func() { sendDone <- h.Send(context.Background(), 1, 7, realtime.Message{Type: "x"}) }()

	select {
	case err := <-sendDone:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Send to another player waited for the old socket to close")
	}
}
