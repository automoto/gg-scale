package managementtoken

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
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
		ScopePlayersRead:      {"project:42:players", rbac.ActionManage},
		ScopePlayersPII:       {"project:42:players", rbac.ActionManage},
	}
	for scope, want := range cases {
		t.Run(scope, func(t *testing.T) {
			got, _ := Scope(scope)

			assert.Equal(t, want, [2]string{got.Pair(42), got.Action})
		})
	}
}

func TestScopes_should_have_a_known_class(t *testing.T) {
	for _, s := range Scopes {
		assert.Contains(t, []int{ClassNormal, ClassRisky}, s.Class, s.Scope)
	}
}

func TestScopes_risky_scopes_should_have_a_label_and_a_known_dependency(t *testing.T) {
	for _, s := range Scopes {
		if s.Class != ClassRisky {
			continue
		}
		dep, ok := Scope(s.Needs)
		assert.NotEmpty(t, s.Label, s.Scope)
		assert.True(t, s.Needs == "" || ok && dep.Class == ClassNormal, s.Scope)
	}
}

func TestScopes_players_pii_should_need_players_read(t *testing.T) {
	got, _ := Scope(ScopePlayersPII)

	assert.Equal(t, []any{ClassRisky, ScopePlayersRead}, []any{got.Class, got.Needs})
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

func TestScopes_should_each_belong_to_a_resource_row(t *testing.T) {
	for _, s := range Scopes {
		ok := slices.ContainsFunc(Resources, func(r Resource) bool { return r.Name == s.Resource })

		assert.True(t, ok, "scope %s has unknown resource %q", s.Scope, s.Resource)
	}
}

func TestResources_read_scope_should_be_a_normal_scope_of_the_row(t *testing.T) {
	for _, r := range Resources {
		s, ok := Scope(r.Read)

		assert.Equal(t, []any{true, r.Name, ClassNormal}, []any{ok, s.Resource, s.Class}, r.Name)
	}
}

func TestResource_write_scopes_should_be_the_other_normal_scopes_of_the_row(t *testing.T) {
	r := resourceNamed("config")

	assert.Equal(t, []string{ScopeConfigWrite, ScopeOriginsWrite}, r.WriteScopes())
}

func TestResource_should_have_no_write_scopes_for_matchmaking(t *testing.T) {
	r := resourceNamed("matchmaking")

	assert.Empty(t, r.WriteScopes())
}

func TestReadScopes_should_list_the_read_scope_of_each_row(t *testing.T) {
	got := ReadScopes()

	assert.Equal(t, []string{ScopeConfigRead, ScopeLeaderboardsRead, ScopeKeysRead, ScopeMatchmakingRead, ScopePlayersRead}, got)
}

func resourceNamed(name string) Resource {
	i := slices.IndexFunc(Resources, func(r Resource) bool { return r.Name == name })
	return Resources[i]
}
