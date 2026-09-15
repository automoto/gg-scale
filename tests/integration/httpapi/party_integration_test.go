//go:build integration

// e2e:bucket b

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/httpapi"
	"github.com/automoto/gg-scale/internal/matchmaker"
	"github.com/automoto/gg-scale/internal/party"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
	"github.com/automoto/gg-scale/internal/tenant"
	"github.com/stretchr/testify/assert"
)

func TestPartyAPIQueueAndRematchExcludeSoloFill(t *testing.T) {
	c := startCluster(t)
	tenantID, projectID := seedTenantWithAPIKey(t, c.bootstrapPool, 0, "party-api")
	_, err := c.bootstrapPool.Exec(t.Context(), `UPDATE api_keys SET scopes=ARRAY['matchmaker'],key_type='publishable' WHERE tenant_id=$1 AND project_id=$2`, tenantID, projectID)
	if !assert.NoError(t, err) {
		return
	}
	signer, err := auth.NewSigner([]byte(testSignerKey))
	if !assert.NoError(t, err) {
		return
	}
	pool := db.NewPool(c.appPool)
	authorizer, err := rbac.NewAuthorizer(pool)
	if !assert.NoError(t, err) {
		return
	}
	t.Cleanup(authorizer.Close)
	q := matchmaker.NewPGQueue(pool)
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{Pool: pool, Lookup: tenant.NewSQLLookup(c.appPool), Limiter: ratelimit.NewCacheLimiter(c.cache), Signer: signer, Cache: c.cache, RBAC: authorizer, Matchmaker: q, PartyEnqueueEnabled: true, MatchmakerTicketTTL: time.Minute}))
	t.Cleanup(srv.Close)
	leader, _ := anonymousLoginWithID(t, srv.URL, "party-api")
	friend, friendID := anonymousLoginWithID(t, srv.URL, "party-api")
	fill, _ := anonymousLoginWithID(t, srv.URL, "party-api")
	request := func(method, path, token, key string, body any, out any) bool {
		raw, err := json.Marshal(body)
		if !assert.NoError(t, err) {
			return false
		}
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(raw))
		if !assert.NoError(t, err) {
			return false
		}
		req.Header.Set("Authorization", "Bearer party-api")
		req.Header.Set("X-Session-Token", token)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if !assert.NoError(t, err) {
			return false
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if !assert.NoError(t, err) {
			return false
		}
		if !assert.Less(t, resp.StatusCode, 300, string(data)) {
			return false
		}
		return out == nil || assert.NoError(t, json.Unmarshal(data, out))
	}
	settings := map[string]any{"mode": "match_only", "min_count": 3, "max_count": 3, "count_multiple": 1}
	var p party.Party
	if !request("POST", "/v1/parties", leader, "", map[string]any{"settings": settings}, &p) {
		return
	}
	path := fmt.Sprintf("/v1/parties/%d", p.ID)
	var code party.Code
	if !request("POST", path+"/invite-codes", leader, "", map[string]any{"expected_version": p.Version, "max_uses": 1}, &code) {
		return
	}
	if !request("POST", "/v1/parties/join", friend, "", map[string]any{"code": code.Code, "expected_version": code.PartyVersion}, &p) {
		return
	}
	for _, token := range []string{leader, friend} {
		if !request("PUT", path+"/members/me/ready", token, "", map[string]any{"expected_version": p.Version, "ready": true, "properties": map[string]any{}}, &p) {
			return
		}
	}
	if !request("POST", path+"/queue", leader, "first", map[string]any{"expected_version": p.Version}, &p) {
		return
	}
	if !request("POST", "/v1/matchmaker/tickets", fill, "", settings, nil) {
		return
	}
	worker := matchmaker.NewWorker(q, nil, nil, matchmaker.WorkerConfig{})
	if !assert.NoError(t, worker.Tick(context.Background())) {
		return
	}
	if !request("GET", "/v1/parties/current", friend, "", nil, &p) {
		return
	}
	assert.Equal(t, "matched", p.State)
	assert.Len(t, p.Members, 2)
	previous := p.LastMatchID
	var ownTicket int64
	for _, m := range p.Members {
		if m.PlayerID == friendID {
			ownTicket = m.TicketID
		}
	}
	var ticket struct {
		Users []matchmaker.RosterEntry `json:"users"`
	}
	if !request("GET", fmt.Sprintf("/v1/matchmaker/tickets/%d", ownTicket), friend, "", nil, &ticket) {
		return
	}
	assert.Len(t, ticket.Users, 3, "polling recovers the missed realtime roster")
	for _, token := range []string{leader, friend} {
		if !request("PUT", path+"/members/me/ready", token, "", map[string]any{"expected_version": p.Version, "ready": true, "properties": map[string]any{}}, &p) {
			return
		}
	}
	if !request("POST", path+"/rematch", leader, "again", map[string]any{"expected_version": p.Version, "last_match_id": previous}, &p) {
		return
	}
	assert.Equal(t, "queued", p.State)
	assert.Len(t, p.Members, 2)
}
