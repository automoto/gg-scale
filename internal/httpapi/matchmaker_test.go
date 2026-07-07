package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ggscale/ggscale/internal/db"
	"github.com/ggscale/ggscale/internal/matchmaker"
	"github.com/ggscale/ggscale/internal/playerauth"
	"github.com/ggscale/ggscale/internal/ratelimit"
	"github.com/ggscale/ggscale/internal/rbac"
	"github.com/ggscale/ggscale/internal/tenant"
)

type allowAllLimiter struct{}

func (allowAllLimiter) Allow(context.Context, string, float64, float64) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true}, nil
}

// matchmakerTestRouter mounts the matchmaker routes behind stub tenant +
// player context injection so scope/feature gating can be exercised
// without Postgres.
func matchmakerTestRouter(t *testing.T, d Deps, key tenant.APIKey) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := db.WithTenant(req.Context(), key.TenantID)
				ctx = db.WithProject(ctx, 7)
				ctx = playerauth.WithID(ctx, 42)
				ctx = tenant.WithAPIKey(ctx, key)
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Group(func(r chi.Router) {
			r.Use(tenant.RequireKeyScope(tenant.ScopeMatchmaker))
			registerMatchmakerRoutes(groupAPI(r, newHumaConfig("test")), d)
		})
	})
	return r
}

func matchmakerTestDeps(t *testing.T) Deps {
	t.Helper()
	auth, err := rbac.NewMemoryAuthorizer()
	require.NoError(t, err)
	return Deps{
		Matchmaker: matchmaker.NewMemQueue(),
		RBAC:       auth,
		Limiter:    allowAllLimiter{},
	}
}

func postTicket(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/matchmaker/tickets/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMatchmakerRoutes_should_403_when_key_lacks_matchmaker_scope(t *testing.T) {
	d := matchmakerTestDeps(t)
	key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeFleet}}
	h := matchmakerTestRouter(t, d, key)

	rec := postTicket(t, h, `{"fleet":"arena"}`)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestMatchmakerRoutes_should_pass_scope_gate_with_matchmaker_scope(t *testing.T) {
	d := matchmakerTestDeps(t)
	key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeMatchmaker}}
	h := matchmakerTestRouter(t, d, key)

	req := httptest.NewRequest(http.MethodGet, "/v1/matchmaker/tickets/999", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Past the scope gate: 404 (unknown ticket), not 403.
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMatchmakerCreate_should_403_fleet_ticket_without_fleet_scope(t *testing.T) {
	d := matchmakerTestDeps(t)
	key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeMatchmaker}}
	h := matchmakerTestRouter(t, d, key)

	rec := postTicket(t, h, `{"fleet":"arena"}`)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestMatchmakerCreate_match_only_should_succeed_without_fleet_backend(t *testing.T) {
	d := matchmakerTestDeps(t)
	key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeMatchmaker}}
	h := matchmakerTestRouter(t, d, key)

	rec := postTicket(t, h, `{"mode":"match_only","region":"eu-1","game_mode":"1v1","min_count":2,"max_count":4}`)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "match_only", resp["mode"])
	assert.Equal(t, "queued", resp["status"])
	assert.Equal(t, float64(2), resp["min_count"])
	assert.Equal(t, float64(4), resp["max_count"])
}

func TestMatchmakerCreate_should_infer_mode_from_fleet_presence(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"no fleet infers match_only", `{"region":"eu-1","game_mode":"1v1"}`, http.StatusCreated},
		{"fleet infers fleet_allocation and hits fleet gating", `{"fleet":"arena"}`, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := matchmakerTestDeps(t)
			key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeMatchmaker}}
			h := matchmakerTestRouter(t, d, key)
			rec := postTicket(t, h, c.body)
			assert.Equal(t, c.want, rec.Code, rec.Body.String())
		})
	}
}

func TestMatchmakerCreate_should_validate_counts_and_mode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"min greater than max", `{"mode":"match_only","min_count":4,"max_count":2}`, http.StatusBadRequest},
		{"negative min", `{"mode":"match_only","min_count":-1}`, http.StatusBadRequest},
		{"count_multiple with no feasible size", `{"mode":"match_only","min_count":3,"max_count":3,"count_multiple":2}`, http.StatusBadRequest},
		{"count_multiple feasible", `{"mode":"match_only","min_count":3,"max_count":4,"count_multiple":2}`, http.StatusCreated},
		{"unknown mode", `{"mode":"warp_drive"}`, http.StatusBadRequest},
		{"fleet field on match_only", `{"mode":"match_only","fleet":"arena"}`, http.StatusBadRequest},
		{"allow_cross_region on fleet mode", `{"mode":"fleet_allocation","fleet":"arena","allow_cross_region":false}`, http.StatusBadRequest},
		{"defaults applied", `{"mode":"match_only"}`, http.StatusCreated},
		{"game_session mode accepted", `{"mode":"game_session","game_mode":"coop"}`, http.StatusCreated},
		{"valid query accepted", `{"mode":"match_only","query":"region:eu AND rank>=5","numeric_properties":{"rank":7}}`, http.StatusCreated},
		{"invalid query rejected", `{"mode":"match_only","query":"rank>>5"}`, http.StatusBadRequest},
		{"reserved property key rejected", `{"mode":"match_only","string_properties":{"region":"eu"}}`, http.StatusBadRequest},
		{"uppercase property key rejected", `{"mode":"match_only","numeric_properties":{"Rank":5}}`, http.StatusBadRequest},
		{"number-like property key rejected", `{"mode":"match_only","numeric_properties":{"123":5}}`, http.StatusBadRequest},
		{"max_count overflowing int32 rejected", `{"mode":"match_only","max_count":2147483648}`, http.StatusBadRequest},
		{"game_session within player cap accepted", `{"mode":"game_session","game_mode":"coop","min_count":2,"max_count":64}`, http.StatusCreated},
		{"game_session above player cap rejected", `{"mode":"game_session","game_mode":"coop","max_count":65}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := matchmakerTestDeps(t)
			key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeMatchmaker}}
			h := matchmakerTestRouter(t, d, key)
			rec := postTicket(t, h, c.body)
			assert.Equal(t, c.want, rec.Code, rec.Body.String())
		})
	}
}

func TestMatchmakerCreate_should_enforce_concurrent_ticket_limit(t *testing.T) {
	d := matchmakerTestDeps(t)
	d.MatchmakerMaxTicketsPerPlayer = 2
	key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeMatchmaker}}
	h := matchmakerTestRouter(t, d, key)

	for i := 0; i < 2; i++ {
		rec := postTicket(t, h, `{"mode":"match_only"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	rec := postTicket(t, h, `{"mode":"match_only"}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}

func TestMatchmakerGet_should_return_roster_after_match_for_missed_event_recovery(t *testing.T) {
	d := matchmakerTestDeps(t)
	key := tenant.APIKey{TenantID: 1, Scopes: []string{tenant.ScopeMatchmaker}}
	h := matchmakerTestRouter(t, d, key)

	rec := postTicket(t, h, `{"mode":"match_only","region":"eu-1","game_mode":"1v1"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// Match the ticket with no realtime hub attached — the WS event is
	// "missed" by construction; polling must still recover the result.
	w := matchmaker.NewWorker(d.Matchmaker, nil, nil, matchmaker.WorkerConfig{})
	require.NoError(t, w.Tick(context.Background()))

	req := httptest.NewRequest(http.MethodGet, "/v1/matchmaker/tickets/"+strconv.FormatInt(created.ID, 10), nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, req)

	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	var resp struct {
		Status  string `json:"status"`
		MatchID string `json:"match_id"`
		Users   []struct {
			PlayerID int64 `json:"player_id"`
		} `json:"users"`
	}
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &resp))
	assert.Equal(t, "matched", resp.Status)
	assert.True(t, strings.HasPrefix(resp.MatchID, "mm_"), "match_id=%q", resp.MatchID)
	require.Len(t, resp.Users, 1)
	assert.Equal(t, int64(42), resp.Users[0].PlayerID)
}
