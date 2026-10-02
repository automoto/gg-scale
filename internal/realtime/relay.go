package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"time"
)

// relayChannel is the Postgres NOTIFY channel that carries messages between
// server hosts. Postgres has no per-channel privileges, so any database role
// can LISTEN or NOTIFY on it. That is acceptable while only the server's own
// roles have database credentials.
const relayChannel = "realtime_relay"

// maxRelayPayload keeps a relayed message under the Postgres NOTIFY limit of
// 8000 bytes.
const maxRelayPayload = 7900

// relayIDBytes is the most an int64 player ID and its comma take in JSON.
const relayIDBytes = 21

const relayMaxBackoff = 30 * time.Second

// relayQueueSize bounds the relayed frames waiting for one player. When the
// socket is too slow to keep up, newer frames are dropped; clients poll.
const relayQueueSize = 64

// relayQueueIdle is how long a player's relay queue goroutine waits for a
// new frame before it exits.
const relayQueueIdle = 10 * time.Second

// ErrRelayTooLarge is returned when the player is on another host and the
// message is too large for a Postgres NOTIFY.
var ErrRelayTooLarge = errors.New("realtime: message too large to relay")

// Relay carries messages to the other server hosts. *db.Pool implements it.
// Use the primary database: a replica does not deliver NOTIFY.
type Relay interface {
	Notify(ctx context.Context, channel, payload string) error
	ListenChannel(ctx context.Context, channel string, fn func(payload string)) error
}

type relayEnvelope struct {
	TenantID  int64   `json:"t"`
	PlayerIDs []int64 `json:"p"`
	Message   Message `json:"m"`
}

// WithRelay lets Push reach players whose socket is on another host. Run
// RunRelay on every host to deliver the messages that other hosts relay.
func (h *Hub) WithRelay(r Relay) *Hub {
	h.relay = r
	return h
}

// Push sends msg to one player. See PushMany.
func (h *Hub) Push(ctx context.Context, tenantID, playerID int64, msg Message) error {
	return h.PushMany(ctx, tenantID, []int64{playerID}, msg)
}

// PushMany sends msg to each player's socket on this host, and relays it
// for the other players in as few NOTIFYs as the size limit allows. A relay
// does not confirm delivery: a nil error means delivered here or relayed.
// Callers that must know about delivery use Send.
func (h *Hub) PushMany(ctx context.Context, tenantID int64, playerIDs []int64, msg Message) error {
	var errs []error
	var remote []int64
	for _, id := range slices.Compact(slices.Sorted(slices.Values(playerIDs))) {
		err := h.Send(ctx, tenantID, id, msg)
		if errors.Is(err, ErrNotConnected) {
			remote = append(remote, id)
			continue
		}
		errs = append(errs, err)
	}
	if len(remote) == 0 {
		return errors.Join(errs...)
	}
	if h.relay == nil {
		return errors.Join(append(errs, ErrNotConnected)...)
	}
	return errors.Join(append(errs, h.relayTo(ctx, tenantID, remote, msg))...)
}

func (h *Hub) relayTo(ctx context.Context, tenantID int64, playerIDs []int64, msg Message) error {
	empty, err := json.Marshal(relayEnvelope{TenantID: tenantID, Message: msg})
	if err != nil {
		return err
	}
	perNotify := (maxRelayPayload - len(empty)) / relayIDBytes
	if perNotify < 1 {
		return ErrRelayTooLarge
	}
	for chunk := range slices.Chunk(playerIDs, perNotify) {
		raw, err := json.Marshal(relayEnvelope{TenantID: tenantID, PlayerIDs: chunk, Message: msg})
		if err != nil {
			return err
		}
		if err := h.relay.Notify(ctx, relayChannel, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// RunRelay delivers messages relayed by other hosts to the sockets on this
// host. It reconnects with backoff until ctx is done; the backoff resets after
// a connection that stayed up longer than the longest wait. A message relayed
// while the listener reconnects is lost; clients poll for the state they need.
func (h *Hub) RunRelay(ctx context.Context) {
	backoff := time.Second
	for {
		started := time.Now()
		err := h.relay.ListenChannel(ctx, relayChannel, func(payload string) {
			h.deliverRelayed(ctx, payload)
		})
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > relayMaxBackoff {
			backoff = time.Second
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

// deliverRelayed queues each frame for its player. One goroutine per player
// writes the queue in order, so one slow socket (a write can take up to
// writeTimeout) does not hold up the listener or the other players.
func (h *Hub) deliverRelayed(ctx context.Context, payload string) {
	var env relayEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		slog.WarnContext(ctx, "realtime relay: malformed payload", "err", err)
		return
	}
	data, err := json.Marshal(env.Message)
	if err != nil {
		return
	}
	for _, id := range env.PlayerIDs {
		if _, ok := h.writer(env.TenantID, id); ok {
			h.enqueueRelayed(ctx, connKey{env.TenantID, id}, data)
		}
	}
}

// enqueueRelayed adds data to the player's queue and starts its writer
// goroutine if none runs. The send happens under h.mu, the same lock the
// writer goroutine holds when it decides to exit, so no frame is left in a
// queue without a goroutine.
func (h *Hub) enqueueRelayed(ctx context.Context, key connKey, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	q, ok := h.queues[key]
	if !ok {
		q = make(chan []byte, relayQueueSize)
		h.queues[key] = q
		go h.drainRelayed(ctx, key, q)
	}
	select {
	case q <- data:
	default:
		slog.WarnContext(ctx, "realtime relay: queue full, frame dropped", "player_id", key.playerID)
	}
}

func (h *Hub) drainRelayed(ctx context.Context, key connKey, q chan []byte) {
	for {
		select {
		case data := <-q:
			w, ok := h.writer(key.tenantID, key.playerID)
			if !ok {
				continue
			}
			if err := w.Write(ctx, data); err != nil {
				slog.WarnContext(ctx, "realtime relay: deliver failed", "player_id", key.playerID, "err", err)
			}
		case <-time.After(relayQueueIdle):
			h.mu.Lock()
			if len(q) > 0 {
				h.mu.Unlock()
				continue
			}
			delete(h.queues, key)
			h.mu.Unlock()
			return
		case <-ctx.Done():
			return
		}
	}
}
