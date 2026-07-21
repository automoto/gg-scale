package entitlement

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testToken = "0123456789abcdef0123456789abcdef"

func newTestHandler() http.Handler {
	return New(Deps{Token: testToken})
}

func doRequest(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_rejects_missing_bearer_token(t *testing.T) {
	rec := doRequest(t, newTestHandler(), http.MethodGet, "/1", "", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestHandler_rejects_wrong_bearer_token(t *testing.T) {
	rec := doRequest(t, newTestHandler(), http.MethodGet, "/1", "wrong-token-wrong-token-wrong-tok", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestPut_rejects_invalid_tenant_id(t *testing.T) {
	rec := doRequest(t, newTestHandler(), http.MethodPut, "/abc", testToken, `{"tier":1,"features":[]}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPut_rejects_malformed_json(t *testing.T) {
	rec := doRequest(t, newTestHandler(), http.MethodPut, "/1", testToken, `{`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPut_rejects_out_of_range_tier(t *testing.T) {
	rec := doRequest(t, newTestHandler(), http.MethodPut, "/1", testToken, `{"tier":5,"features":[]}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestPut_rejects_unmanaged_feature(t *testing.T) {
	rec := doRequest(t, newTestHandler(), http.MethodPut, "/1", testToken, `{"tier":1,"features":["matchmaker"]}`)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestValidateState(t *testing.T) {
	tests := []struct {
		name    string
		state   State
		wantErr bool
	}{
		{"tier 0 no features", State{Tier: 0}, false},
		{"tier 3 both features", State{Tier: 3, Features: []string{"p2p_relay", "dedicated_servers"}}, false},
		{"negative tier", State{Tier: -1}, true},
		{"tier above ladder", State{Tier: 4}, true},
		{"unknown feature", State{Tier: 1, Features: []string{"time_travel"}}, true},
		{"fleet backend features are not billing-managed", State{Tier: 1, Features: []string{"fleet_docker_backend"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateState(tt.state)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestDiffStates(t *testing.T) {
	tests := []struct {
		name        string
		currentTier int
		current     []string
		desired     State
		wantChanged bool
		wantEnable  []string
		wantDisable []string
	}{
		{
			name:        "identical state is a no-op",
			currentTier: 2, current: []string{"p2p_relay"},
			desired: State{Tier: 2, Features: []string{"p2p_relay"}},
		},
		{
			name:        "tier change only",
			currentTier: 1, current: nil,
			desired:     State{Tier: 2},
			wantChanged: true,
		},
		{
			name:        "feature enable",
			currentTier: 1, current: nil,
			desired:     State{Tier: 1, Features: []string{"p2p_relay"}},
			wantChanged: true,
			wantEnable:  []string{"p2p_relay"},
		},
		{
			name:        "feature disable",
			currentTier: 1, current: []string{"p2p_relay", "dedicated_servers"},
			desired:     State{Tier: 1, Features: []string{"dedicated_servers"}},
			wantChanged: true,
			wantDisable: []string{"p2p_relay"},
		},
		{
			name:        "unmanaged current grants are left alone",
			currentTier: 1, current: []string{"fleet_docker_backend"},
			desired: State{Tier: 1},
		},
		{
			name:        "duplicate desired features are deduped",
			currentTier: 1, current: nil,
			desired:     State{Tier: 1, Features: []string{"p2p_relay", "p2p_relay"}},
			wantChanged: true,
			wantEnable:  []string{"p2p_relay"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := diffStates(tt.currentTier, tt.current, tt.desired)
			assert.Equal(t, tt.wantChanged, d.changed(), "changed")
			assert.ElementsMatch(t, tt.wantEnable, d.enable, "enable")
			assert.ElementsMatch(t, tt.wantDisable, d.disable, "disable")
		})
	}
}
