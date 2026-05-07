package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/fleet"
)

func authedRequest(t *testing.T, method, path string, body []byte, tenantID, projectID int64, urlParams ...[2]string) *http.Request {
	t.Helper()
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, http.NoBody)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	}
	ctx := db.WithTenant(req.Context(), tenantID)
	if projectID != 0 {
		ctx = db.WithProject(ctx, projectID)
	}
	if len(urlParams) > 0 {
		rctx := chi.NewRouteContext()
		for _, kv := range urlParams {
			rctx.URLParams.Add(kv[0], kv[1])
		}
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

func TestFleetRegister_creates_server_and_returns_id(t *testing.T) {
	reg := fleet.NewRegistry(30 * time.Second)
	d := Deps{Fleet: reg}

	body, _ := json.Marshal(fleetRegisterRequest{
		Name: "doomerang-1", Address: "localhost:7373", Version: "0.1.0", MaxPlayers: 4,
	})
	req := authedRequest(t, http.MethodPost, "/v1/fleet/servers", body, 1, 10)
	rec := httptest.NewRecorder()

	fleetRegisterHandler(d).ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var got map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	_, err := uuid.Parse(got["id"])
	assert.NoError(t, err)
	assert.Len(t, reg.List(1, 10), 1)
}

func TestFleetRegister_requires_project_pin(t *testing.T) {
	reg := fleet.NewRegistry(30 * time.Second)
	d := Deps{Fleet: reg}

	body, _ := json.Marshal(fleetRegisterRequest{Address: "localhost:7373"})
	req := authedRequest(t, http.MethodPost, "/v1/fleet/servers", body, 1, 0)
	rec := httptest.NewRecorder()

	fleetRegisterHandler(d).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFleetRegister_rejects_empty_address(t *testing.T) {
	reg := fleet.NewRegistry(30 * time.Second)
	d := Deps{Fleet: reg}

	body, _ := json.Marshal(fleetRegisterRequest{Address: ""})
	req := authedRequest(t, http.MethodPost, "/v1/fleet/servers", body, 1, 10)
	rec := httptest.NewRecorder()

	fleetRegisterHandler(d).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFleetHeartbeat_refreshes_ttl(t *testing.T) {
	reg := fleet.NewRegistry(30 * time.Second)
	id, err := reg.Register(fleet.RegisterParams{TenantID: 1, ProjectID: 10, Address: "h:1"})
	require.NoError(t, err)
	d := Deps{Fleet: reg}

	req := authedRequest(t, http.MethodPut, "/v1/fleet/servers/"+id.String()+"/heartbeat", nil, 1, 0, [2]string{"id", id.String()})
	rec := httptest.NewRecorder()

	fleetHeartbeatHandler(d).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestFleetHeartbeat_unknown_id_404(t *testing.T) {
	reg := fleet.NewRegistry(30 * time.Second)
	d := Deps{Fleet: reg}
	id := uuid.New().String()

	req := authedRequest(t, http.MethodPut, "/v1/fleet/servers/"+id+"/heartbeat", nil, 1, 0, [2]string{"id", id})
	rec := httptest.NewRecorder()

	fleetHeartbeatHandler(d).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestFleetDeregister_removes_server(t *testing.T) {
	reg := fleet.NewRegistry(30 * time.Second)
	id, err := reg.Register(fleet.RegisterParams{TenantID: 1, ProjectID: 10, Address: "h:1"})
	require.NoError(t, err)
	d := Deps{Fleet: reg}

	req := authedRequest(t, http.MethodDelete, "/v1/fleet/servers/"+id.String(), nil, 1, 0, [2]string{"id", id.String()})
	rec := httptest.NewRecorder()

	fleetDeregisterHandler(d).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, reg.List(1, 10))
}

func TestFleetList_returns_active_servers_for_project(t *testing.T) {
	reg := fleet.NewRegistry(30 * time.Second)
	_, err := reg.Register(fleet.RegisterParams{TenantID: 1, ProjectID: 10, Name: "a", Address: "h:1"})
	require.NoError(t, err)
	_, err = reg.Register(fleet.RegisterParams{TenantID: 1, ProjectID: 11, Name: "b", Address: "h:2"})
	require.NoError(t, err)
	d := Deps{Fleet: reg}

	req := authedRequest(t, http.MethodGet, "/v1/fleet/servers", nil, 1, 10)
	rec := httptest.NewRecorder()

	fleetListHandler(d).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Servers []fleetServerView `json:"servers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Servers, 1)
	assert.Equal(t, "a", got.Servers[0].Name)
}

func TestFleetList_empty_when_fleet_disabled(t *testing.T) {
	d := Deps{Fleet: nil}

	req := authedRequest(t, http.MethodGet, "/v1/fleet/servers", nil, 1, 10)
	rec := httptest.NewRecorder()

	fleetListHandler(d).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Servers []fleetServerView `json:"servers"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.Servers)
}
