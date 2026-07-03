//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFriends_block_enforcement_e2e is the end-to-end blocking assertion:
// request → accept → block → the blocked player's re-request is
// refused and a game invite to them is rejected → unblock → interaction
// restored. Blocking never reveals itself to the blockee.
func TestFriends_block_enforcement_e2e(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, "free", "k")
	srv := newServerForCluster(t, c)

	tokA, idA := anonymousLoginWithID(t, srv.URL, "k")
	tokB, idB := anonymousLoginWithID(t, srv.URL, "k")
	setPlayerEmail(t, c, idB, "blockb@example.com")
	// makeFriends links both accounts and establishes an accepted friendship.
	makeFriends(t, c, srv.URL, "k", idA, tokA, idB, tokB)

	// A blocks B.
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/block", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	// The accepted friendship is gone (block overwrote it): A's accepted list
	// is empty.
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/friends/?status=accepted", "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"items":[]`)

	// B cannot re-friend A, and the refusal never reveals the block: it is
	// indistinguishable from a request to a non-existent target (404).
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idA), "k", tokB, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))

	// B must not learn who blocked them: B's blocked list is empty (only the
	// blocker sees the edge).
	resp, body = authedReq(t, http.MethodGet, srv.URL+"/v1/friends/?status=blocked", "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"items":[]`)

	// A game invite from A to B is rejected (no accepted friendship).
	sess := createSession(t, srv.URL, "k", tokA, 4)
	resp, body = authedReq(t, http.MethodPost, srv.URL+"/v1/invite", "k", tokA,
		map[string]string{"to_email": "blockb@example.com", "session_id": sess.SessionID})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, string(body))

	// A unblocks B; they can be friends again.
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/unblock", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/request", srv.URL, idB), "k", tokA, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/friends/%d/accept", srv.URL, idA), "k", tokB, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}
