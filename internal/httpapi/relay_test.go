package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/matchmaker"
)

func humaStatus(t *testing.T, err error) int {
	t.Helper()
	require.Error(t, err)
	se, ok := err.(huma.StatusError)
	require.True(t, ok, "error is not a huma.StatusError: %v", err)
	return se.GetStatus()
}

func TestVerifyRelayMatchMembership(t *testing.T) {
	q := matchmaker.NewMemQueue()
	tctx := db.WithTenant(context.Background(), 1)
	require.NoError(t, q.CreateMatch(tctx, &matchmaker.Match{
		ID:        "mm_room",
		TenantID:  1,
		ProjectID: 7,
		Roster:    []matchmaker.RosterEntry{{PlayerID: 41}, {PlayerID: 42}},
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}))
	d := Deps{Matchmaker: q}

	t.Run("member is allowed", func(t *testing.T) {
		assert.NoError(t, verifyRelayMatchMembership(tctx, d, 7, 42, "mm_room"))
	})
	t.Run("non-member is forbidden", func(t *testing.T) {
		assert.Equal(t, 403, humaStatus(t, verifyRelayMatchMembership(tctx, d, 7, 99, "mm_room")))
	})
	t.Run("cross-project match is forbidden", func(t *testing.T) {
		assert.Equal(t, 403, humaStatus(t, verifyRelayMatchMembership(tctx, d, 8, 41, "mm_room")))
	})
	t.Run("unknown match is forbidden, not enumerable", func(t *testing.T) {
		assert.Equal(t, 403, humaStatus(t, verifyRelayMatchMembership(tctx, d, 7, 41, "mm_missing")))
	})
	t.Run("no matchmaker configured", func(t *testing.T) {
		assert.Equal(t, 400, humaStatus(t, verifyRelayMatchMembership(tctx, Deps{}, 7, 41, "mm_room")))
	})
}
