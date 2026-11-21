package managementtoken

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/automoto/gg-scale/internal/cache/memory"
	"github.com/automoto/gg-scale/internal/ratelimit"
	"github.com/automoto/gg-scale/internal/rbac"
)

func TestScopes_should_use_only_project_level_casbin_objects(t *testing.T) {
	for _, s := range Scopes {
		obj := s.Pair(42)
		assert.True(t, strings.HasPrefix(obj, "project:42:") || obj == rbac.ObjectProject || obj == rbac.ObjectAPIKeyPublic,
			"scope %s uses %s", s.Scope, obj)
	}
}

func TestScopes_read_scopes_should_use_the_pair_of_the_dashboard_page(t *testing.T) {
	cases := map[string][2]string{
		ScopeConfigRead:       {rbac.ObjectProject, rbac.ActionManage},
		ScopeLeaderboardsRead: {rbac.ObjectProject, rbac.ActionManage},
		ScopeKeysRead:         {rbac.ObjectProject, rbac.ActionManage},
		ScopeMatchmakingRead:  {"project:42:matchmaker", rbac.ActionRead},
	}
	for scope, want := range cases {
		t.Run(scope, func(t *testing.T) {
			got, _ := Scope(scope)

			assert.Equal(t, want, [2]string{got.Pair(42), got.Action})
		})
	}
}

func TestScopes_should_all_be_normal_class(t *testing.T) {
	for _, s := range Scopes {
		assert.Equal(t, ClassNormal, s.Class, s.Scope)
	}
}

func TestScope_should_return_the_pair_of_the_scope(t *testing.T) {
	got, ok := Scope(ScopeLeaderboardsWrite)

	assert.Equal(t, []any{true, "project:42:leaderboard", rbac.ActionManage}, []any{ok, got.Pair(42), got.Action})
}

func TestScope_should_report_an_unknown_scope(t *testing.T) {
	_, ok := Scope("players:delete")

	assert.False(t, ok)
}

func TestBearerToken(t *testing.T) {
	cases := map[string]struct {
		header string
		token  string
		ok     bool
	}{
		"valid":          {"Bearer ggm_abc", "ggm_abc", true},
		"lower case":     {"bearer ggm_abc", "ggm_abc", true},
		"empty":          {"", "", false},
		"only prefix":    {"Bearer ", "", false},
		"only spaces":    {"Bearer    ", "", false},
		"other scheme":   {"Basic ggm_abc", "", false},
		"no space":       {"Bearerggm_abc", "", false},
		"trailing space": {"Bearer ggm_abc ", "ggm_abc", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			token, ok := bearerToken(c.header)

			assert.Equal(t, []any{c.token, c.ok}, []any{token, ok})
		})
	}
}

func serveOrigin(origin string) int {
	h := SameOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "http://api.example.com/mcp", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestSameOrigin(t *testing.T) {
	cases := map[string]struct {
		origin string
		code   int
	}{
		"no origin":      {"", http.StatusOK},
		"same host":      {"https://api.example.com", http.StatusOK},
		"same host case": {"https://API.example.com", http.StatusOK},
		"other host":     {"https://evil.example", http.StatusForbidden},
		"null":           {"null", http.StatusForbidden},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.code, serveOrigin(c.origin))
		})
	}
}

func TestFromContext_should_return_the_principal_of_NewContext(t *testing.T) {
	ctx := NewContext(context.Background(), Principal{TokenID: 9})

	p, ok := FromContext(ctx)

	assert.Equal(t, []any{int64(9), true}, []any{p.TokenID, ok})
}

func TestFromContext_should_report_no_principal(t *testing.T) {
	_, ok := FromContext(context.Background())

	assert.False(t, ok)
}

func TestMiddleware_should_send_bearer_challenge_when_header_is_missing(t *testing.T) {
	h := Middleware(Deps{Limiter: ratelimit.NewCacheLimiter(memory.New())}, "api")(http.NotFoundHandler())
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, []any{http.StatusUnauthorized, "Bearer"}, []any{rec.Code, rec.Header().Get("WWW-Authenticate")})
}
