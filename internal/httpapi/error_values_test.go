package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/automoto/gg-scale/internal/auth"
	"github.com/automoto/gg-scale/internal/db"
	"github.com/automoto/gg-scale/internal/managementtoken"
)

// probeMarker is a text that no error answer holds unless it copies the
// request.
const probeMarker = "PROBEMARKER7Q3Z"

// probeServer serves every operation of the OpenAPI document and /mcp as the
// router does, with zero Deps and no auth middleware. Each probe is refused
// by request validation, so no handler runs.
type probeServer struct {
	h   http.Handler
	api huma.API
	doc *huma.OpenAPI
}

func newProbeServer(t *testing.T) probeServer {
	t.Helper()
	cfg := newHumaConfig("test")
	var d Deps
	d.ControlPanel.FleetEnabled = true
	var api huma.API
	r := chi.NewRouter()
	r.Use(panicRecover())
	r.Route("/v1", func(r chi.Router) {
		api = groupAPI(r, cfg)
		registerDocOperations(api, d)
	})
	r.Group(func(r chi.Router) {
		r.Use(strictToolArguments(cfg.OpenAPI))
		attachManagementMCP(groupAPI(r, cfg), "test")
	})
	ctx := managementtoken.NewContext(context.Background(), managementtoken.Principal{Scopes: managementtoken.ScopeNames()})
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { r.ServeHTTP(w, req.WithContext(ctx)) })
	return probeServer{h: h, api: api, doc: cfg.OpenAPI}
}

func (s probeServer) do(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec
}

// mcp sends one tools/call and returns the raw JSON-RPC answer.
func (s probeServer) mcp(tool, args string) string {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	return s.mcpRaw(body)
}

func (s probeServer) mcpRaw(body string) string {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec.Body.String()
}

// operations returns every operation of the document.
func (s probeServer) operations() []*huma.Operation {
	var ops []*huma.Operation
	for _, item := range s.doc.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if op != nil {
				ops = append(ops, op)
			}
		}
	}
	return ops
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// secretFieldNames are the body properties that hold a password, a token, or
// a code.
var secretFieldNames = []string{"password", "current_password", "new_password", "refresh_token", "token", "code", "session_token", "ticket"}

type secretField struct {
	op    *huma.Operation
	field string
}

// secretFields returns each top-level string body property of an operation
// whose name is in secretFieldNames.
func (s probeServer) secretFields() []secretField {
	var out []secretField
	for _, op := range s.operations() {
		if op.RequestBody == nil || op.RequestBody.Content["application/json"] == nil {
			continue
		}
		schema := op.RequestBody.Content["application/json"].Schema
		if schema != nil && schema.Ref != "" {
			schema = s.doc.Components.Schemas.SchemaFromRef(schema.Ref)
		}
		if schema == nil {
			continue
		}
		for name, prop := range schema.Properties {
			if slices.Contains(secretFieldNames, name) && prop.Type == huma.TypeString {
				out = append(out, secretField{op: op, field: name})
			}
		}
	}
	return out
}

func TestSecretFields_should_cover_the_player_auth_operations(t *testing.T) {
	s := newProbeServer(t)

	got := map[string]bool{}
	for _, f := range s.secretFields() {
		got[f.op.OperationID] = true
	}

	for _, id := range []string{"authSignup", "authLogin", "authLogout", "authRefresh", "authCustomToken", "authSteam",
		"authVerify", "authDeleteCancel", "changePassword", "confirmPasswordReset", "disablePlayer", "linkEmail",
		"linkSteam", "requestPlayerDelete"} {
		assert.True(t, got[id], id)
	}
}

func TestValidationError_should_not_return_a_secret_field_value(t *testing.T) {
	s := newProbeServer(t)
	probes := map[string]func(field string) string{
		"wrong_type":   func(field string) string { return `{"` + field + `":["` + probeMarker + `"]}` },
		"empty_string": func(field string) string { return `{"` + field + `":"","probe_extra":"` + probeMarker + `"}` },
	}
	for _, f := range s.secretFields() {
		path := pathParam.ReplaceAllString(f.op.Path, "1")
		for name, body := range probes {
			t.Run(f.op.OperationID+"/"+f.field+"/"+name, func(t *testing.T) {
				rec := s.do(f.op.Method, path, body(f.field))

				assert.Equal(t, []any{http.StatusUnprocessableEntity, false, false},
					[]any{rec.Code, strings.Contains(rec.Body.String(), probeMarker), strings.Contains(rec.Body.String(), `"value"`)},
					rec.Body.String())
			})
		}
	}
}

// managementProbes are refused management calls whose arguments hold the
// marker: the Steam key, a plugin config value, an invite address, and a key
// label.
var managementProbes = []struct {
	tool, method, path, args string
}{
	{"set_steam_auth", http.MethodPut, "/v1/project/steam-auth", `{"app_id":"480","web_api_key":["` + probeMarker + `"]}`},
	{"set_steam_auth", http.MethodPut, "/v1/project/steam-auth", `{"web_api_key":"` + probeMarker + `"}`},
	{"create_fleet", http.MethodPost, "/v1/project/fleets", `{"backend":"plugin:x","config":{"api_token":"` + probeMarker + `"}}`},
	{"create_fleet", http.MethodPost, "/v1/project/fleets", `{"name":"a","backend":"plugin:x","config":{"api_token":["` + probeMarker + `"]}}`},
	{"update_fleet", http.MethodPut, "/v1/project/fleets/1", `{"config":{"api_token":["` + probeMarker + `"]}}`},
	{"invite_player", http.MethodPost, "/v1/project/players/invite", `{"email":["` + probeMarker + `@example.com"]}`},
	{"create_api_key", http.MethodPost, "/v1/project/api-keys", `{"label":["` + probeMarker + `"]}`},
}

func TestManagementValidationError_should_not_return_the_value_over_http(t *testing.T) {
	s := newProbeServer(t)
	for _, p := range managementProbes {
		t.Run(p.tool+" "+p.args, func(t *testing.T) {
			rec := s.do(p.method, p.path, p.args)

			assert.Equal(t, []any{http.StatusUnprocessableEntity, false, false},
				[]any{rec.Code, strings.Contains(rec.Body.String(), probeMarker), strings.Contains(rec.Body.String(), `"value"`)},
				rec.Body.String())
		})
	}
}

func TestManagementValidationError_should_not_return_the_value_over_mcp(t *testing.T) {
	s := newProbeServer(t)
	for _, p := range managementProbes {
		t.Run(p.tool+" "+p.args, func(t *testing.T) {
			args := p.args
			if p.tool == "update_fleet" {
				args = `{"fleet_id":1,` + strings.TrimPrefix(args, "{")
			}

			got := s.mcp(p.tool, args)

			assert.Equal(t, []any{true, false, false},
				[]any{strings.Contains(got, `\"status\":422`), strings.Contains(got, probeMarker), strings.Contains(got, `\"value\"`)}, got)
		})
	}
}

func TestBodyParseError_should_not_return_body_text(t *testing.T) {
	s := newProbeServer(t)
	// The first bad byte is the marker character.
	cases := []struct{ name, path string }{
		{"player_auth", "/v1/auth/login"},
		{"management", "/v1/project/api-keys"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := s.do(http.MethodPost, c.path, `{"password":`+"^"+probeMarker+`}`)

			assert.Equal(t, []any{http.StatusBadRequest, false, false},
				[]any{rec.Code, strings.Contains(rec.Body.String(), "^"), strings.Contains(rec.Body.String(), probeMarker)},
				rec.Body.String())
		})
	}
}

func TestBodyParseError_should_say_the_body_could_not_be_decoded(t *testing.T) {
	s := newProbeServer(t)

	rec := s.do(http.MethodPost, "/v1/auth/login", `{"password":^`)

	assert.Contains(t, rec.Body.String(), `"message":"request body could not be decoded"`)
}

func TestMCPEnvelopeParseError_should_not_return_body_text(t *testing.T) {
	// The JSON-RPC envelope does not parse, so huma mcp answers before any
	// tool body is decoded.
	s := newProbeServer(t)

	got := s.mcpRaw(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_api_key","arguments":{"label":^` + probeMarker + `}}}`)

	assert.Equal(t, []any{false, false}, []any{strings.Contains(got, "^"), strings.Contains(got, probeMarker)}, got)
}

func TestBodyParseError_should_not_return_a_duplicate_key_body_over_mcp(t *testing.T) {
	s := newProbeServer(t)

	got := s.mcp("create_fleet", `{"name":"a","backend":"plugin:x","config":{"api_token":"`+probeMarker+`","api_token":"b"}}`)

	assert.Equal(t, []any{true, false}, []any{strings.Contains(got, "request body could not be decoded"), strings.Contains(got, probeMarker)}, got)
}

// The session verify route decodes its own body and answers one opaque 401
// for every failure.
func TestBodyDecodeError_should_not_return_body_text_for_valid_json_of_the_wrong_size(t *testing.T) {
	s := newProbeServer(t)

	// The JSON is valid and passes validation, but 1e30 does not fit the
	// int64 of expected_revision, so the decode into the input fails.
	rec := s.do(http.MethodPut, "/v1/project/remote-config", `{"config":{},"expected_revision":1e30}`)

	assert.Equal(t, []any{http.StatusUnprocessableEntity, false, true},
		[]any{rec.Code, strings.Contains(rec.Body.String(), "1e30"), strings.Contains(rec.Body.String(), "request body could not be decoded")},
		rec.Body.String())
}

func TestVerifyPlayerSession_should_not_return_the_session_token(t *testing.T) {
	signer, err := auth.NewSignerRandom()
	require.NoError(t, err)
	d := Deps{Signer: signer}
	ctx := db.WithProject(db.WithTenant(context.Background(), 1), 1)
	bodies := map[string]string{
		"wrong_type":   `{"session_token":["` + probeMarker + `"]}`,
		"empty_string": `{"session_token":"","probe_extra":"` + probeMarker + `"}`,
		"bad_token":    `{"session_token":"` + probeMarker + `"}`,
		"not_json":     `{"session_token":^` + probeMarker + `}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/server/player-sessions/verify", strings.NewReader(body)).WithContext(ctx)
			rec := httptest.NewRecorder()

			verifyPlayerSession(d, rec, req)

			assert.Equal(t, []any{http.StatusUnauthorized, `{"error":"invalid session"}`}, []any{rec.Code, rec.Body.String()})
		})
	}
}

func TestErrorValues_should_be_dropped_for_every_operation(t *testing.T) {
	s := newProbeServer(t)
	for _, op := range s.operations() {
		t.Run(op.OperationID, func(t *testing.T) {
			// Arrange
			ctx := humatest.NewContext(op, httptest.NewRequest(op.Method, "/", nil), httptest.NewRecorder())
			model := huma.NewError(http.StatusUnprocessableEntity, "validation failed",
				&huma.ErrorDetail{Location: "body.x", Value: probeMarker}).(*huma.ErrorModel)

			// Act
			out, err := s.api.Transform(ctx, "422", model)

			// Assert
			require.NoError(t, err)
			assert.Nil(t, out.(*huma.ErrorModel).Errors[0].Value)
		})
	}
}

func TestDropErrorValues(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
		kept     bool
	}{
		{"should_keep_the_active_ticket_id_of_a_409", http.StatusConflict, "active_ticket_id", true},
		{"should_drop_another_location_of_a_409", http.StatusConflict, "body.active_ticket_id", false},
		{"should_drop_the_active_ticket_id_of_a_422", http.StatusUnprocessableEntity, "active_ticket_id", false},
		{"should_drop_a_body_value_of_a_422", http.StatusUnprocessableEntity, "body.password", false},
		{"should_drop_the_whole_body_of_a_400", http.StatusBadRequest, "body", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := huma.NewError(c.status, "x", &huma.ErrorDetail{Location: c.location, Value: int64(55)}).(*huma.ErrorModel)

			_, err := dropErrorValues(nil, "", model)

			require.NoError(t, err)
			assert.Equal(t, c.kept, model.Errors[0].Value != nil)
		})
	}
}

func TestDropErrorValues_should_skip_a_nil_detail(t *testing.T) {
	model := huma.NewError(http.StatusBadRequest, "x", nil).(*huma.ErrorModel)

	_, err := dropErrorValues(nil, "", model)

	assert.NoError(t, err)
}

// leakyFormats are the huma string formats whose error message holds the
// parse error of the value, and so part of the value.
var leakyFormats = []string{"email", "idn-email", "duration", "uri", "iri", "uri-reference", "iri-reference", "uri-template", "uuid", "regex"}

// schemaFormats returns every format in schema and the schemas it reaches.
func schemaFormats(reg huma.Registry, schema *huma.Schema, seen map[*huma.Schema]bool) []string {
	if schema == nil {
		return nil
	}
	if schema.Ref != "" {
		schema = reg.SchemaFromRef(schema.Ref)
	}
	if schema == nil || seen[schema] {
		return nil
	}
	seen[schema] = true
	out := []string{schema.Format}
	children := []*huma.Schema{schema.Items, schema.Not}
	children = append(children, schema.OneOf...)
	children = append(children, schema.AnyOf...)
	children = append(children, schema.AllOf...)
	for _, p := range schema.Properties {
		children = append(children, p)
	}
	if extra, ok := schema.AdditionalProperties.(*huma.Schema); ok {
		children = append(children, extra)
	}
	for _, c := range children {
		out = append(out, schemaFormats(reg, c, seen)...)
	}
	return out
}

func TestRequestSchemas_should_have_no_format_that_puts_the_value_in_the_message(t *testing.T) {
	s := newProbeServer(t)
	for _, op := range s.operations() {
		// Arrange
		var formats []string
		for _, p := range op.Parameters {
			formats = append(formats, schemaFormats(s.doc.Components.Schemas, p.Schema, map[*huma.Schema]bool{})...)
		}
		if op.RequestBody != nil {
			for _, mt := range op.RequestBody.Content {
				formats = append(formats, schemaFormats(s.doc.Components.Schemas, mt.Schema, map[*huma.Schema]bool{})...)
			}
		}

		// Act
		leaky := slices.DeleteFunc(formats, func(f string) bool { return !slices.Contains(leakyFormats, f) })

		// Assert
		assert.Empty(t, leaky, op.OperationID)
	}
}

func TestDeleteCancel_should_refuse_a_bad_email_without_returning_it(t *testing.T) {
	s := newProbeServer(t)

	rec := s.do(http.MethodPost, "/v1/auth/delete/cancel", `{"email":"a@b `+probeMarker+`","password":"correct-horse"}`)

	assert.Equal(t, []any{http.StatusUnprocessableEntity, false, true},
		[]any{rec.Code, strings.Contains(rec.Body.String(), probeMarker), strings.Contains(rec.Body.String(), `"location":"body.email"`)}, rec.Body.String())
}

func TestErrorValues_should_keep_the_json_shape(t *testing.T) {
	s := newProbeServer(t)

	rec := s.do(http.MethodPost, "/v1/project/api-keys", `{"label":["`+probeMarker+`"]}`)

	var got struct {
		Errors []map[string]any `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), rec.Body.String())
	assert.Equal(t, "body.label", got.Errors[0]["location"])
}
