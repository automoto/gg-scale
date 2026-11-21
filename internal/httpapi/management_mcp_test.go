package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/managementtoken"
)

func TestManagementToolFilter(t *testing.T) {
	tool := func(scope string) *huma.Operation {
		return &huma.Operation{Metadata: map[string]any{managementToolMetadata: "x", managementScopeMetadata: scope}}
	}
	anyTool := &huma.Operation{Metadata: map[string]any{managementToolMetadata: "x", managementScopeMetadata: "",
		managementAnyScopeMetadata: []string{managementtoken.ScopeConfigRead, managementtoken.ScopeLeaderboardsRead}}}
	withScopes := func(scopes ...string) context.Context {
		return managementtoken.NewContext(context.Background(), managementtoken.Principal{Scopes: scopes})
	}
	cases := []struct {
		name string
		ctx  context.Context
		op   *huma.Operation
		want bool
	}{
		{"should_hide_an_operation_with_no_row", withScopes(), &huma.Operation{}, false},
		{"should_hide_a_tool_without_a_principal", context.Background(), tool(""), false},
		{"should_show_a_tool_with_no_scope", withScopes(), tool(""), true},
		{"should_hide_a_tool_whose_scope_the_token_lacks", withScopes(managementtoken.ScopeKeysCreate), tool(managementtoken.ScopeConfigWrite), false},
		{"should_show_a_tool_whose_scope_the_token_has", withScopes(managementtoken.ScopeConfigWrite), tool(managementtoken.ScopeConfigWrite), true},
		{"should_show_a_tool_when_the_token_has_one_of_its_scopes", withScopes(managementtoken.ScopeLeaderboardsRead), anyTool, true},
		{"should_hide_a_tool_when_the_token_has_none_of_its_scopes", withScopes(managementtoken.ScopeKeysRead), anyTool, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := managementToolFilter(c.ctx, c.op)

			assert.Equal(t, c.want, got)
		})
	}
}

// strictCall sends body through strictToolArguments and returns whether the
// next handler ran and the response body.
func strictCall(t *testing.T, body string) (bool, string) {
	t.Helper()
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	h := strictToolArguments(OpenAPIDoc("1.0.0"))(next)
	ctx := managementtoken.NewContext(context.Background(), managementtoken.Principal{Scopes: []string{managementtoken.ScopeLeaderboardsRead, managementtoken.ScopeLeaderboardsWrite}})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return reached, rec.Body.String()
}

func TestStrictToolArguments_should_pass_known_arguments(t *testing.T) {
	reached, _ := strictCall(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leaderboard","arguments":{"leaderboard_id":1}}}`)

	assert.True(t, reached)
}

func TestStrictToolArguments_should_refuse_unknown_argument_after_a_duplicate_params_key(t *testing.T) {
	reached, body := strictCall(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":5,`+
		`"params":{"name":"update_leaderboard","arguments":{"leaderboard_id":1,"metadata_json":"{}"}}}`)

	assert.Equal(t, []any{false, true}, []any{reached, strings.Contains(body, "metadata_json")}, body)
}

func TestStrictToolArguments_should_refuse_empty_path_argument(t *testing.T) {
	reached, body := strictCall(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leaderboard","arguments":{"leaderboard_id":""}}}`)

	assert.Equal(t, []any{false, true}, []any{reached, strings.Contains(body, "leaderboard_id")}, body)
}

func TestStrictToolArguments_should_refuse_unknown_argument_in_a_body_with_vertical_tab(t *testing.T) {
	reached, _ := strictCall(t, "\v"+`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leaderboard","arguments":{"leaderboard_id":1,"x":1}}}`)

	assert.False(t, reached)
}

// allScopes returns every scope, so the tool filter shows every
// tool.
func allScopes() []string {
	scopes := []string{}
	for _, s := range managementtoken.Scopes {
		scopes = append(scopes, s.Scope)
	}
	return scopes
}

// listedToolArguments sends tools/list to an MCP endpoint wired as in the
// router and returns the sorted inputSchema property names of each tool.
func listedToolArguments(t *testing.T, cfg huma.Config) map[string][]string {
	t.Helper()
	r := chi.NewRouter()
	registerProjectManagementRoutes(groupAPI(r, cfg), Deps{})
	attachManagementMCP(groupAPI(r, cfg), "test")
	ctx := managementtoken.NewContext(context.Background(), managementtoken.Principal{Scopes: allScopes()})
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { r.ServeHTTP(w, req.WithContext(ctx)) })

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var out struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	got := map[string][]string{}
	for _, tool := range out.Result.Tools {
		got[tool.Name] = slices.Sorted(maps.Keys(tool.InputSchema.Properties))
	}
	return got
}

func TestManagementToolArguments_should_match_the_tools_list_input_schemas(t *testing.T) {
	// Arrange
	cfg := newHumaConfig("test")
	listed := listedToolArguments(t, cfg)

	// Act
	want := map[string][]string{}
	for name, tool := range managementToolArguments(cfg.OpenAPI) {
		want[name] = slices.Sorted(maps.Keys(tool.args))
	}

	// Assert
	require.NotEmpty(t, listed)
	assert.Equal(t, want, listed)
}

func TestStrictToolArguments_should_pass_on(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"should_pass_a_notification_with_an_unknown_argument", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"get_leaderboard","arguments":{"leaderboard_id":1,"x":1}}}`},
		{"should_pass_null_arguments", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leaderboard","arguments":null}}`},
		{"should_pass_arguments_that_are_not_an_object", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leaderboard","arguments":[1]}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached, body := strictCall(t, c.body)

			assert.True(t, reached, body)
		})
	}
}

func TestStrictToolArguments_should_restore_the_body_for_the_next_handler(t *testing.T) {
	// Arrange
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leaderboard","arguments":{"leaderboard_id":1}}}`
	var got []byte
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got, _ = io.ReadAll(r.Body) })
	h := strictToolArguments(OpenAPIDoc("1.0.0"))(next)
	ctx := managementtoken.NewContext(context.Background(), managementtoken.Principal{Scopes: allScopes()})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body)).WithContext(ctx)

	// Act
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Assert
	assert.Equal(t, body, string(got))
}

func TestStrictToolArguments_should_refuse_path_argument_that_huma_joins_to_empty(t *testing.T) {
	for _, value := range []string{`[]`, `[""]`, `[" "]`} {
		reached, _ := strictCall(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_leaderboard","arguments":{"leaderboard_id":`+value+`}}}`)

		assert.False(t, reached, value)
	}
}
