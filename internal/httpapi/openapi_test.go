package httpapi

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// expectedV1Paths is the full set of documented /v1 paths. If a register* call
// is dropped from OpenAPIDoc (or added without updating it), this fails —
// guarding the generated spec against silent drift from the handlers.
var expectedV1Paths = []string{
	"/v1/account/remote-addrs",
	"/v1/auth/anonymous",
	"/v1/auth/custom-token",
	"/v1/auth/login",
	"/v1/auth/logout",
	"/v1/auth/refresh",
	"/v1/auth/signup",
	"/v1/auth/verify",
	"/v1/fleets/heartbeat",
	"/v1/fleets/{fleet}/servers",
	"/v1/friends",
	"/v1/friends/{player_id}",
	"/v1/friends/{player_id}/accept",
	"/v1/friends/{player_id}/block",
	"/v1/friends/{player_id}/reject",
	"/v1/friends/{player_id}/remote-addrs",
	"/v1/friends/{player_id}/request",
	"/v1/friends/{player_id}/unblock",
	"/v1/game-session",
	"/v1/game-session/{id}",
	"/v1/game-session/{id}/heartbeat",
	"/v1/game-session/{id}/join",
	"/v1/healthz",
	"/v1/invite",
	"/v1/invite/{id}",
	"/v1/leaderboards/{id}/around-me",
	"/v1/leaderboards/{id}/scores",
	"/v1/leaderboards/{id}/top",
	"/v1/matchmaker/tickets",
	"/v1/matchmaker/tickets/{id}",
	"/v1/presence",
	"/v1/profile",
	"/v1/relay/credentials",
	"/v1/server/player-sessions/verify",
	"/v1/server/players/{player_id}/remote-addrs",
	"/v1/storage/objects",
	"/v1/storage/objects/{key}",
	"/v1/ws",
}

func TestOpenAPIDoc_covers_expected_paths(t *testing.T) {
	doc := OpenAPIDoc("1.0.0")
	require.NotNil(t, doc)

	got := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		got = append(got, p)
	}
	sort.Strings(got)

	want := append([]string(nil), expectedV1Paths...)
	sort.Strings(want)

	assert.Equal(t, want, got, "documented /v1 paths drifted from OpenAPIDoc registrations")
}

func TestOpenAPIDoc_verify_stays_api_key_only_and_documented(t *testing.T) {
	doc := OpenAPIDoc("1.0.0")
	item := doc.Paths["/v1/server/player-sessions/verify"]
	require.NotNil(t, item)
	require.NotNil(t, item.Post)

	// Opaque-401 endpoint: api-key auth only, and the body-callback op is
	// enriched with request + 200/401 response schemas.
	require.Len(t, item.Post.Security, 1)
	_, hasAPIKey := item.Post.Security[0]["ApiKeyAuth"]
	_, hasPlayer := item.Post.Security[0]["PlayerSession"]
	assert.True(t, hasAPIKey)
	assert.False(t, hasPlayer, "verify must not require a player session")

	require.NotNil(t, item.Post.RequestBody)
	assert.Contains(t, item.Post.Responses, "200")
	assert.Contains(t, item.Post.Responses, "401")
}
