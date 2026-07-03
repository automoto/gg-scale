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

// TestE2E_MatchmakerDeliversMatchReadyOverWebSocket exercises the realtime
// + matchmaker integration without Postgres: a client opens /v1/ws, POSTs a
// ticket to /v1/matchmaker/tickets, and asserts the worker fans a
// MatchReady envelope back to the WS connection.
//
// This is the M6/M7 acceptance shape — Postgres-backed and Docker-backed
// versions live in build-tagged integration tests (matchmaker_integration_test.go,
// to be added when CI provisions Postgres for matchmaker).
func TestE2EMatchmakerDeliversMatchReadyOverWebSocket(t *testing.T) {
	hub := realtime.NewHub()
	queue := matchmaker.NewMemQueue()
	alloc := &fakeAllocator{address: "10.42.0.7:7777"}

	worker := matchmaker.NewWorker(queue, alloc, hub, matchmaker.WorkerConfig{
		BucketSize: 1,
		Interval:   10 * time.Millisecond,
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

	// Worker should pick the ticket, allocate via fakeAllocator, and push
	// MatchReady over WS within a couple of ticks.
	mt, data, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, mt)
	var msg realtime.Message
	require.NoError(t, json.Unmarshal(data, &msg))
	assert.Equal(t, "match_ready", msg.Type)

	var payload struct {
		Address  string `json:"address"`
		TicketID int64  `json:"ticket_id"`
	}
	require.NoError(t, json.Unmarshal(msg.Payload, &payload))
	assert.Equal(t, "10.42.0.7:7777", payload.Address)
	assert.NotZero(t, payload.TicketID)

	// fakeAllocator should have been called with the right tenant + region.
	require.Equal(t, int64(1), alloc.Called())
	require.Len(t, alloc.gotReqs, 1)
	assert.Equal(t, tenantID, alloc.gotReqs[0].TenantID)
	assert.Equal(t, projectID, alloc.gotReqs[0].ProjectID)
	assert.Equal(t, "us-east-1", alloc.gotReqs[0].Region)
}
