//go:build integration

package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type signalPollResponse struct {
	Signals []struct {
		ID            int64  `json:"id"`
		FromPlayerID  int64  `json:"from_player_id"`
		ToPlayerID    int64  `json:"to_player_id"`
		NegotiationID string `json:"negotiation_id"`
		Kind          string `json:"kind"`
		Payload       string `json:"payload"`
	} `json:"signals"`
}

func TestGameSessionSignals_areRecipientOnlyAndCursorBound(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv := newServerForCluster(t, c)

	hostToken, hostID := anonymousLoginWithID(t, srv.URL, "k")
	guestToken, guestID := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", hostToken, 2)
	resp, body := authedReq(t, http.MethodPost,
		fmt.Sprintf("%s/v1/game-session/%s/join", srv.URL, sess.SessionID), "k", guestToken,
		map[string]any{"public_addr": addr("5.6.7.8", 9001)})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	signalURL := fmt.Sprintf("%s/v1/game-session/%s/signals", srv.URL, sess.SessionID)
	resp, body = authedReq(t, http.MethodPost, signalURL, "k", hostToken, map[string]any{
		"to_player_id": guestID, "negotiation_id": "neg-1", "kind": "offer", "payload": "v=0",
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	var sent struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &sent))
	require.Positive(t, sent.ID)

	resp, body = authedReq(t, http.MethodGet, signalURL, "k", guestToken, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var polled signalPollResponse
	require.NoError(t, json.Unmarshal(body, &polled))
	require.Len(t, polled.Signals, 1)
	assert.Equal(t, hostID, polled.Signals[0].FromPlayerID)
	assert.Equal(t, guestID, polled.Signals[0].ToPlayerID)
	assert.Equal(t, "neg-1", polled.Signals[0].NegotiationID)
	assert.Equal(t, "offer", polled.Signals[0].Kind)
	assert.Equal(t, "v=0", polled.Signals[0].Payload)

	resp, body = authedReq(t, http.MethodGet,
		fmt.Sprintf("%s?after_id=%d", signalURL, sent.ID), "k", guestToken, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &polled))
	assert.Empty(t, polled.Signals)

	resp, body = authedReq(t, http.MethodGet, signalURL, "k", hostToken, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &polled))
	assert.Empty(t, polled.Signals, "sender must not read a recipient's signal")
}

func TestGameSessionSignals_rejectNonMembers(t *testing.T) {
	c := startCluster(t)
	seedTenantWithAPIKey(t, c.bootstrapPool, 0, "k")
	srv := newServerForCluster(t, c)

	hostToken, hostID := anonymousLoginWithID(t, srv.URL, "k")
	strangerToken, _ := anonymousLoginWithID(t, srv.URL, "k")
	sess := createSession(t, srv.URL, "k", hostToken, 2)
	signalURL := fmt.Sprintf("%s/v1/game-session/%s/signals", srv.URL, sess.SessionID)

	resp, body := authedReq(t, http.MethodGet, signalURL, "k", strangerToken, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
	resp, body = authedReq(t, http.MethodPost, signalURL, "k", strangerToken, map[string]any{
		"to_player_id": hostID, "negotiation_id": "neg-1", "kind": "answer", "payload": "v=0",
	})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(body))
}
