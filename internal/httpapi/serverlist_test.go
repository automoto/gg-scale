package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/serverlist"
)

func newServerListTestDeps() Deps {
	return Deps{ServerList: serverlist.New(30 * time.Second)}
}

func TestFleetHeartbeatHandler_StoresHeartbeat(t *testing.T) {
	d := newServerListTestDeps()
	h := fleetHeartbeatHandler(d)

	body := `{
		"agones_name": "gs-1",
		"fleet": "doomerang-east",
		"address": "10.0.0.1:7777",
		"region": "us-east",
		"name": "Doomerang Server",
		"current_players": 2,
		"max_players": 4,
		"game_mode": "deathmatch",
		"level": "arena_battle_starter",
		"version": "v0.2.0"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/fleets/heartbeat", strings.NewReader(body))
	req = req.WithContext(db.WithTenant(context.Background(), 1))
	rw := httptest.NewRecorder()
	h(rw, req)

	require.Equal(t, http.StatusNoContent, rw.Code, "expected 204; body=%s", rw.Body.String())
	got := d.ServerList.List(1, "doomerang-east")
	require.Len(t, got, 1)
	assert.Equal(t, "Doomerang Server", got[0].Name)
	assert.Equal(t, 2, got[0].CurrentPlayers)
}

// Regression: the heartbeat body cannot override the authenticated
// tenant. Without this, a tenant-1 server could pose as tenant-2's fleet.
func TestFleetHeartbeatHandler_TenantFromContextNotBody(t *testing.T) {
	d := newServerListTestDeps()
	h := fleetHeartbeatHandler(d)

	// Body has no tenant_id field — but even if a client tried to
	// inject one, the handler ignores it.
	body := `{"agones_name":"gs-1","fleet":"f","address":"a","name":"n","max_players":4}`
	req := httptest.NewRequest(http.MethodPost, "/h", strings.NewReader(body))
	req = req.WithContext(db.WithTenant(context.Background(), 7))
	rw := httptest.NewRecorder()
	h(rw, req)

	require.Equal(t, http.StatusNoContent, rw.Code)
	assert.Empty(t, d.ServerList.List(1, "f"), "must not appear under tenant 1")
	assert.Len(t, d.ServerList.List(7, "f"), 1, "must appear under tenant 7 (from ctx)")
}

func TestFleetHeartbeatHandler_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"missing agones_name", `{"fleet":"f","address":"a","max_players":4}`, http.StatusBadRequest},
		{"missing fleet", `{"agones_name":"g","address":"a","max_players":4}`, http.StatusBadRequest},
		{"missing address", `{"agones_name":"g","fleet":"f","max_players":4}`, http.StatusBadRequest},
		{"max_players zero", `{"agones_name":"g","fleet":"f","address":"a","max_players":0}`, http.StatusBadRequest},
		{"current_players negative", `{"agones_name":"g","fleet":"f","address":"a","max_players":4,"current_players":-1}`, http.StatusBadRequest},
		{"current_players over cap", `{"agones_name":"g","fleet":"f","address":"a","max_players":4,"current_players":5}`, http.StatusBadRequest},
		{"invalid json", `not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newServerListTestDeps()
			req := httptest.NewRequest(http.MethodPost, "/h", strings.NewReader(tc.body))
			req = req.WithContext(db.WithTenant(context.Background(), 1))
			rw := httptest.NewRecorder()
			fleetHeartbeatHandler(d)(rw, req)
			assert.Equal(t, tc.code, rw.Code)
		})
	}
}

func TestFleetServersListHandler_ReturnsLiveServers(t *testing.T) {
	d := newServerListTestDeps()
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-1", Fleet: "doomerang-east", Address: "10.0.0.1:7777",
		Region: "us-east", Name: "A", CurrentPlayers: 1, MaxPlayers: 4, TenantID: 1,
	})
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-2", Fleet: "doomerang-east", Address: "10.0.0.2:7778",
		Region: "us-east", Name: "B", CurrentPlayers: 3, MaxPlayers: 4, TenantID: 1,
	})

	r := chi.NewRouter()
	r.Get("/v1/fleets/{fleet}/servers", fleetServersListHandler(d))

	req := httptest.NewRequest(http.MethodGet, "/v1/fleets/doomerang-east/servers", nil)
	req = req.WithContext(db.WithTenant(context.Background(), 1))
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)

	require.Equal(t, http.StatusOK, rw.Code)
	var resp listServersResponse
	require.NoError(t, json.NewDecoder(rw.Body).Decode(&resp))
	require.Len(t, resp.Servers, 2)
	assert.Equal(t, "A", resp.Servers[0].Name)
	assert.Equal(t, "B", resp.Servers[1].Name)
}

func TestFleetServersListHandler_TenantIsolation(t *testing.T) {
	d := newServerListTestDeps()
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-1", Fleet: "f", Address: "a", Name: "tenant-1-server", MaxPlayers: 4, TenantID: 1,
	})
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-1", Fleet: "f", Address: "a", Name: "tenant-2-server", MaxPlayers: 4, TenantID: 2,
	})

	r := chi.NewRouter()
	r.Get("/v1/fleets/{fleet}/servers", fleetServersListHandler(d))

	req := httptest.NewRequest(http.MethodGet, "/v1/fleets/f/servers", nil)
	req = req.WithContext(db.WithTenant(context.Background(), 1))
	rw := httptest.NewRecorder()
	r.ServeHTTP(rw, req)

	var resp listServersResponse
	require.NoError(t, json.NewDecoder(rw.Body).Decode(&resp))
	require.Len(t, resp.Servers, 1)
	assert.Equal(t, "tenant-1-server", resp.Servers[0].Name,
		"tenant 1 must only see its own servers")
}

// When ServerList is nil (operator-disabled), both endpoints return 503
// instead of nil-dereferencing.
func TestServerListHandlers_503WhenNotConfigured(t *testing.T) {
	d := Deps{ServerList: nil}

	for _, h := range []http.HandlerFunc{
		fleetHeartbeatHandler(d),
		fleetServersListHandler(d),
	} {
		req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(nil))
		req = req.WithContext(db.WithTenant(context.Background(), 1))
		rw := httptest.NewRecorder()
		h(rw, req)
		assert.Equal(t, http.StatusServiceUnavailable, rw.Code)
	}
}
