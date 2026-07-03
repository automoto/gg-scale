// Package realtime is the WebSocket fan-out for ggscale. The Hub keeps a
// (tenant, player) → live socket map so the matchmaker can push MatchReady
// envelopes to a specific player without knowing the underlying transport.
//
// Transport detail (coder/websocket, framing, heartbeat) lives in server.go;
// hub.go is transport-agnostic and is tested with a fake Writer.
package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

// ErrNotConnected is returned by Send when no socket is registered for the
// target (tenant, player) pair. Callers (matchmaker) treat this as a
// signal the player has disconnected and should retry later or fail the
// ticket.
var ErrNotConnected = errors.New("realtime: player not connected")

// Message is the wire envelope. Type discriminates payloads (match_ready,
// presence, chat …). Payload is opaque JSON.
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Writer is the transport seam. A real *Conn (server.go) writes WebSocket
// text frames; tests inject a recording fake.
type Writer interface {
	Write(ctx context.Context, data []byte) error
	Close() error
}

type connKey struct {
	tenantID int64
	playerID int64
}

// Hub tracks active sockets keyed by (tenant, player). One Hub per
// process; safe for concurrent use.
type Hub struct {
	mu      sync.RWMutex
	writers map[connKey]Writer
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{writers: make(map[connKey]Writer)}
}

// Register attaches w as the active socket for (tenantID, playerID). A
// pre-existing writer in the slot is closed (newer-wins) so a reconnecting
// client doesn't leave stale frames in flight. The returned func removes
// only this specific registration — calling it after a newer Register won't
// evict the newer writer.
func (h *Hub) Register(tenantID, playerID int64, w Writer) func() {
	k := connKey{tenantID, playerID}
	h.mu.Lock()
	if old, ok := h.writers[k]; ok {
		_ = old.Close()
	}
	h.writers[k] = w
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		if cur, ok := h.writers[k]; ok && cur == w {
			delete(h.writers, k)
		}
		h.mu.Unlock()
	}
}

// Send marshals msg as JSON and writes it to the registered socket. Returns
// ErrNotConnected when the target has no live socket.
func (h *Hub) Send(ctx context.Context, tenantID, playerID int64, msg Message) error {
	h.mu.RLock()
	w, ok := h.writers[connKey{tenantID, playerID}]
	h.mu.RUnlock()
	if !ok {
		return ErrNotConnected
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return w.Write(ctx, data)
}

// Count returns the number of registered sockets across all tenants. Used
// by observability and tests.
func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.writers)
}
