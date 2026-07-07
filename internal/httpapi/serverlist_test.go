package httpapi

import (
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

// fleetTestRouter mounts the fleet heartbeat + servers-list huma operations on
// a bare /v1 router so the unit tests can drive them over HTTP.
func fleetTestRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		api := groupAPI(r, newHumaConfig("test"))
		registerFleetHeartbeat(api, d)
		registerFleetServersList(api, d)
	})
	return r
}

func fleetRequest(t *testing.T, h http.Handler, method, target, body string, tenantID int64) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(db.WithTenant(context.Background(), tenantID))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}

func TestFleetHeartbeat_StoresHeartbeat(t *testing.T) {
	d := newServerListTestDeps()
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
	rw := fleetRequest(t, fleetTestRouter(d), http.MethodPost, "/v1/fleets/heartbeat", body, 1)

	require.Equal(t, http.StatusNoContent, rw.Code, "expected 204; body=%s", rw.Body.String())
	got := d.ServerList.List(1, "doomerang-east")
	require.Len(t, got, 1)
	assert.Equal(t, "Doomerang Server", got[0].Name)
	assert.Equal(t, 2, got[0].CurrentPlayers)
}

// Regression: the heartbeat body cannot override the authenticated tenant.
// Without this, a tenant-1 server could pose as tenant-2's fleet.
func TestFleetHeartbeat_TenantFromContextNotBody(t *testing.T) {
	d := newServerListTestDeps()
	body := `{"agones_name":"gs-1","fleet":"f","address":"a","name":"n","max_players":4}`
	rw := fleetRequest(t, fleetTestRouter(d), http.MethodPost, "/v1/fleets/heartbeat", body, 7)

	require.Equal(t, http.StatusNoContent, rw.Code)
	assert.Empty(t, d.ServerList.List(1, "f"), "must not appear under tenant 1")
	assert.Len(t, d.ServerList.List(7, "f"), 1, "must appear under tenant 7 (from ctx)")
}

func TestFleetHeartbeat_RejectsInvalid(t *testing.T) {
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
			rw := fleetRequest(t, fleetTestRouter(d), http.MethodPost, "/v1/fleets/heartbeat", tc.body, 1)
			assert.Equal(t, tc.code, rw.Code, rw.Body.String())
		})
	}
}

func TestFleetServersList_ReturnsLiveServers(t *testing.T) {
	d := newServerListTestDeps()
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-1", Fleet: "doomerang-east", Address: "10.0.0.1:7777",
		Region: "us-east", Name: "A", CurrentPlayers: 1, MaxPlayers: 4, TenantID: 1,
	})
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-2", Fleet: "doomerang-east", Address: "10.0.0.2:7778",
		Region: "us-east", Name: "B", CurrentPlayers: 3, MaxPlayers: 4, TenantID: 1,
	})

	rw := fleetRequest(t, fleetTestRouter(d), http.MethodGet, "/v1/fleets/doomerang-east/servers", "", 1)

	require.Equal(t, http.StatusOK, rw.Code)
	var resp listServersResponse
	require.NoError(t, json.NewDecoder(rw.Body).Decode(&resp))
	require.Len(t, resp.Servers, 2)
	assert.Equal(t, "A", resp.Servers[0].Name)
	assert.Equal(t, "B", resp.Servers[1].Name)
}

func TestFleetServersList_TenantIsolation(t *testing.T) {
	d := newServerListTestDeps()
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-1", Fleet: "f", Address: "a", Name: "tenant-1-server", MaxPlayers: 4, TenantID: 1,
	})
	d.ServerList.Submit(serverlist.Heartbeat{
		AgonesName: "gs-1", Fleet: "f", Address: "a", Name: "tenant-2-server", MaxPlayers: 4, TenantID: 2,
	})

	rw := fleetRequest(t, fleetTestRouter(d), http.MethodGet, "/v1/fleets/f/servers", "", 1)

	var resp listServersResponse
	require.NoError(t, json.NewDecoder(rw.Body).Decode(&resp))
	require.Len(t, resp.Servers, 1)
	assert.Equal(t, "tenant-1-server", resp.Servers[0].Name,
		"tenant 1 must only see its own servers")
}

// When ServerList is nil (operator-disabled), both endpoints return 503
// instead of nil-dereferencing.
func TestServerList_503WhenNotConfigured(t *testing.T) {
	d := Deps{ServerList: nil}
	h := fleetTestRouter(d)

	hb := fleetRequest(t, h, http.MethodPost, "/v1/fleets/heartbeat",
		`{"agones_name":"g","fleet":"f","address":"a","max_players":4}`, 1)
	assert.Equal(t, http.StatusServiceUnavailable, hb.Code, hb.Body.String())

	list := fleetRequest(t, h, http.MethodGet, "/v1/fleets/f/servers", "", 1)
	assert.Equal(t, http.StatusServiceUnavailable, list.Code, list.Body.String())
}
