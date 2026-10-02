package party

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/automoto/gg-scale/internal/realtime"
)

// Realtime event types. An event is a hint: the client gets the party with
// getParty, and the heartbeat still returns the party if an event is lost.
const (
	EventChanged = "party_changed"
	EventInvite  = "party_invite"
)

// Pusher sends a realtime event to players. *realtime.Hub implements it.
type Pusher interface {
	PushMany(ctx context.Context, tenantID int64, playerIDs []int64, msg realtime.Message) error
}

// Change is one committed party write. Players are the members after the
// write and the members it removed, so a removed member learns it is out.
type Change struct {
	TenantID int64
	PartyID  int64
	Version  int64
	State    string
	Players  []int64
}

// writes collects the changes of one transaction. They are sent after the
// commit, so a rolled-back write sends nothing.
type writes struct {
	changes []Change
}

// WithPusher makes the store send party events after each committed write.
func (s *Store) WithPusher(p Pusher) *Store {
	s.pusher = p
	return s
}

// write runs fn in a tenant transaction, or a bootstrap one for the
// cross-tenant sweep, and sends the collected events after the commit.
func (s *Store) write(ctx context.Context, bootstrap bool, fn func(pgx.Tx, *writes) error) error {
	run := s.pool.Q
	if bootstrap {
		run = s.pool.BootstrapQ
	}
	var w writes
	err := run(ctx, func(tx pgx.Tx) error {
		w = writes{}
		return fn(tx, &w)
	})
	if err != nil {
		return err
	}
	PushChanges(ctx, s.pusher, w.changes)
	return nil
}

// PushChanges sends party_changed to each player of each change. The
// matchmaker calls it after a commit that changed party state.
func PushChanges(ctx context.Context, p Pusher, changes []Change) {
	for _, c := range changes {
		payload := map[string]any{"party_id": c.PartyID, "version": c.Version, "state": c.State}
		push(ctx, p, c.TenantID, c.Players, EventChanged, payload)
	}
}

func push(ctx context.Context, p Pusher, tenantID int64, playerIDs []int64, typ string, payload any) {
	if p == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	err = p.PushMany(ctx, tenantID, playerIDs, realtime.Message{Type: typ, Payload: raw})
	if err != nil && !errors.Is(err, realtime.ErrNotConnected) {
		slog.WarnContext(ctx, "party event push failed", "type", typ, "err", err)
	}
}
