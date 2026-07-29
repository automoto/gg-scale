package gamesession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ggscale/ggscale/internal/matchmaker"
)

// sessionCreator is the slice of Service the adapter needs; tests inject a
// fake through it.
type sessionCreator interface {
	Create(ctx context.Context, p CreateParams) (*Created, error)
}

// MatchAdapter adapts Service to the matchmaker's SessionCreator interface.
type MatchAdapter struct {
	svc sessionCreator
}

// NewMatchAdapter returns an adapter over svc.
func NewMatchAdapter(svc *Service) *MatchAdapter {
	return &MatchAdapter{svc: svc}
}

// CreateMatchSession creates a private session sized to the matched roster.
// Every rostered player is pre-seeded as a member (no endpoint yet), so the
// private session admits exactly the matched players; they join and
// heartbeat with their own addresses afterwards. The session starts on the
// short MatchPendingTTL; the first join promotes it to the full lifetime. A
// project at its open-session cap surfaces as matchmaker.ErrCapacity so the
// worker lets the group keep waiting instead of failing it.
func (a *MatchAdapter) CreateMatchSession(ctx context.Context, projectID int64, gameMode string, players []int64) (string, string, error) {
	if len(players) == 0 {
		return "", "", fmt.Errorf("game session: empty roster")
	}
	members := make([]Member, 0, len(players))
	for _, p := range players {
		members = append(members, Member{PlayerID: p})
	}
	props, err := json.Marshal(map[string]any{"game_mode": gameMode, "matchmade": true})
	if err != nil {
		return "", "", err
	}
	created, err := a.svc.Create(ctx, CreateParams{
		ProjectID:    projectID,
		HostPlayerID: players[0],
		Props:        props,
		MaxPlayers:   len(players),
		Private:      true,
		Members:      members,
		TTL:          MatchPendingTTL,
	})
	if errors.Is(err, ErrProjectCapped) {
		return "", "", fmt.Errorf("%w: %w", matchmaker.ErrCapacity, err)
	}
	if err != nil {
		return "", "", err
	}
	return created.SessionID, created.JoinCode, nil
}
