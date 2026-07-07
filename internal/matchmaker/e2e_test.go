package matchmaker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/realtime"
)

// testInject installs (tenantID, playerID) on the request context,
// standing in for the production tenant + player middlewares so the
// e2e test doesn't need Postgres-backed auth.
func testInject(tenantID, playerID, projectID int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = db.WithTenant(ctx, tenantID)
		ctx = db.WithProject(ctx, projectID)
		ctx = playerauth.WithID(ctx, playerID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// TestE2EMatchmakerDeliversMatchedOverWebSocket exercises the realtime +
// matchmaker integration without Postgres or any fleet backend: a client
// opens /v1/ws, POSTs a match_only ticket to /v1/matchmaker/tickets, and
// asserts the worker fans a matchmaker_matched envelope back to the WS
// connection — no allocator involved.
func TestE2EMatchmakerDeliversMatchedOverWebSocket(t *testing.T) {
	hub := realtime.NewHub()
	queue := matchmaker.NewMemQueue()

	worker := matchmaker.NewWorker(queue, nil, hub, matchmaker.WorkerConfig{
		Interval: 10 * time.Millisecond,
	})
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	go worker.Run(workerCtx)

	const (
		tenantID  = int64(1)
		userID    = int64(42)
		projectID = int64(7)
	)

	r := chi.NewRouter()
	r.Get("/v1/ws", realtime.ServeWS(realtime.Options{Hub: hub, HeartbeatInterval: time.Hour}).ServeHTTP)
	r.Post("/v1/matchmaker/tickets", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Region   string `json:"region"`
			GameMode string `json:"game_mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		tid, _ := db.TenantFromContext(r.Context())
		pid, _ := db.ProjectFromContext(r.Context())
		uid, _ := playerauth.IDFromContext(r.Context())
		ticket, err := queue.Enqueue(r.Context(), matchmaker.EnqueueRequest{
			TenantID:  tid,
			ProjectID: pid,
			PlayerID:  uid,
			Region:    req.Region,
			GameMode:  req.GameMode,
		})
		require.NoError(t, err)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": ticket.ID})
	})

	srv := httptest.NewServer(testInject(tenantID, userID, projectID, r))
	defer srv.Close()

	// Connect WS first so the hub has the writer registered before the
	// worker fans the MatchReady.
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	require.Eventually(t, func() bool { return hub.Count() == 1 }, 2*time.Second, 10*time.Millisecond)

	// POST the ticket.
	body := strings.NewReader(`{"region":"us-east-1","game_mode":"1v1"}`)
	resp, err := http.Post(srv.URL+"/v1/matchmaker/tickets", "application/json", body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// Worker should pick the ticket, mint a match, and push
	// matchmaker_matched over WS within a couple of ticks.
	mt, data, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, mt)
	var msg realtime.Message
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "matchmaker_matched", msg.Type)

	var payload struct {
		TicketID int64  `json:"ticket_id"`
		MatchID  string `json:"match_id"`
		Mode     string `json:"mode"`
		Address  string `json:"address"`
		Users    []struct {
			PlayerID int64 `json:"player_id"`
		} `json:"users"`
	}
	require.NoError(t, json.Unmarshal(msg.Payload, &payload))
	assert.NotZero(t, payload.TicketID)
	assert.True(t, strings.HasPrefix(payload.MatchID, "mm_"), "match_id=%q", payload.MatchID)
	assert.Equal(t, "match_only", payload.Mode)
	assert.Empty(t, payload.Address, "match_only carries no server address")
	require.Len(t, payload.Users, 1)
	assert.Equal(t, userID, payload.Users[0].PlayerID)
}
