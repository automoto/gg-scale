package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// relayChannel is the Postgres NOTIFY channel that carries messages between
// server hosts.
const relayChannel = "realtime_relay"

// maxRelayPayload keeps a relayed message under the Postgres NOTIFY limit of
// 8000 bytes.
const maxRelayPayload = 7900

const relayMaxBackoff = 30 * time.Second

// ErrRelayTooLarge is returned by Push when the player is on another host and
// the message is too large for a Postgres NOTIFY.
var ErrRelayTooLarge = errors.New("realtime: message too large to relay")

// Relay carries messages to the other server hosts. *db.Pool implements it.
// Use the primary database: a replica does not deliver NOTIFY.
type Relay interface {
	Notify(ctx context.Context, channel, payload string) error
	ListenChannel(ctx context.Context, channel string, fn func(payload string)) error
}

type relayEnvelope struct {
	TenantID int64   `json:"t"`
	PlayerID int64   `json:"p"`
	Message  Message `json:"m"`
}

// WithRelay lets Push reach players whose socket is on another host. Run
// RunRelay on every host to deliver the messages that other hosts relay.
func (h *Hub) WithRelay(r Relay) *Hub {
	h.relay = r
	return h
}

// Push sends msg to the player's socket on this host. If the socket is not
// here, Push relays msg to the other hosts. A relay does not confirm
// delivery: a nil error means delivered here or relayed. Callers that must
// know about delivery use Send.
func (h *Hub) Push(ctx context.Context, tenantID, playerID int64, msg Message) error {
	err := h.Send(ctx, tenantID, playerID, msg)
	if !errors.Is(err, ErrNotConnected) || h.relay == nil {
		return err
	}
	raw, err := json.Marshal(relayEnvelope{TenantID: tenantID, PlayerID: playerID, Message: msg})
	if err != nil {
		return err
	}
	if len(raw) > maxRelayPayload {
		return ErrRelayTooLarge
	}
	return h.relay.Notify(ctx, relayChannel, string(raw))
}

// RunRelay delivers messages relayed by other hosts to the sockets on this
// host. It reconnects with backoff until ctx is done. A message relayed while
// the listener reconnects is lost; clients poll for the state they need.
func (h *Hub) RunRelay(ctx context.Context) {
	backoff := time.Second
	for {
		err := h.relay.ListenChannel(ctx, relayChannel, func(payload string) {
			h.deliverRelayed(ctx, payload)
		})
		if ctx.Err() != nil {
			return
		}
		slog.WarnContext(ctx, "realtime relay: listener disconnected", "err", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, relayMaxBackoff)
	}
}

func (h *Hub) deliverRelayed(ctx context.Context, payload string) {
	var env relayEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		slog.WarnContext(ctx, "realtime relay: malformed payload", "err", err)
		return
	}
	err := h.Send(ctx, env.TenantID, env.PlayerID, env.Message)
	if err != nil && !errors.Is(err, ErrNotConnected) {
		slog.WarnContext(ctx, "realtime relay: deliver failed", "err", fmt.Errorf("player %d: %w", env.PlayerID, err))
	}
}
