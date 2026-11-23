package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
)

// esc returns a backslash followed by u: esc("ud83d") is the escape text of
// U+D83D. The tests build every escape with it, so the source holds the
// escape text and not a character.
func esc(u string) string { return `\` + u }

func TestJSONFormat_should_check_the_text_of_the_body(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"invalid byte", "\"a\xffb\"", errBodyText},
		{"utf-8 encoded surrogate", "\"\xed\xa0\x80\"", errBodyText},
		{"high surrogate alone", `"` + esc("ud800") + `"`, errBodyText},
		{"low surrogate alone", `"` + esc("udc00") + `"`, errBodyText},
		{"upper-case high surrogate alone", `"` + esc("uD800") + `"`, errBodyText},
		{"high surrogate then text", `"` + esc("ud800") + `x"`, errBodyText},
		{"high surrogate then other escape", `"` + esc("ud800") + `\n"`, errBodyText},
		{"high high low", `"` + esc("ud800") + esc("ud800") + esc("udc00") + `"`, errBodyText},
		{"low then high", `"` + esc("udc00") + esc("ud800") + `"`, errBodyText},
		{"surrogate in a key", `{"` + esc("ud800") + `":1}`, errBodyText},
		{"high surrogate at the end of the body", esc("ud800"), errBodyText},
		{"surrogate pair", `"` + esc("ud83d") + esc("ude00") + `"`, nil},
		{"upper-case surrogate pair", `"` + esc("uD83D") + esc("uDE00") + `"`, nil},
		{"literal replacement character", "\"\xef\xbf\xbd\"", nil},
		{"escaped replacement character", `"` + esc("ufffd") + `"`, nil},
		{"escaped backslash before u", `"\\` + "ud800" + `"`, nil},
		{"four-byte character", "\"\xf0\x9f\x98\x80\"", nil},
		{"invalid json", `{"a":^`, errBodyDecode},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v any

			err := jsonFormat.Unmarshal([]byte(c.body), &v)

			assert.Equal(t, c.want, err)
		})
	}
}

const errBodyTextJSON = `"message":"request body is not valid UTF-8 text"`

// badTextProbes are management calls that are valid except for the text of
// one string: an invalid byte, or a lone surrogate escape.
var badTextProbes = []struct {
	name, tool, method, path, args string
}{
	{"fleet name byte", "create_fleet", http.MethodPost, "/v1/project/fleets", "{\"name\":\"a\xffb\",\"backend\":\"agones\",\"config\":{\"fleet_name\":\"f\"}}"},
	{"region byte", "allocate_server", http.MethodPost, "/v1/project/allocations", "{\"fleet\":\"f\",\"region\":\"e\xffu\"}"},
	{"key label byte", "create_api_key", http.MethodPost, "/v1/project/api-keys", "{\"label\":\"a\xffb\"}"},
	{"key label high surrogate", "create_api_key", http.MethodPost, "/v1/project/api-keys", `{"label":"` + esc("ud800") + `"}`},
	{"key label low surrogate", "create_api_key", http.MethodPost, "/v1/project/api-keys", `{"label":"` + esc("udc00") + `"}`},
}

// The probe server has no store, so a refused call shows that no handler ran
// and nothing was stored.
func TestBadText_should_be_refused_over_http(t *testing.T) {
	s := newProbeServer(t)
	for _, p := range badTextProbes {
		t.Run(p.name, func(t *testing.T) {
			rec := s.do(p.method, p.path, p.args)

			assert.Equal(t, []any{http.StatusBadRequest, true, false},
				[]any{rec.Code, strings.Contains(rec.Body.String(), errBodyTextJSON), strings.Contains(rec.Body.String(), `"value"`)},
				rec.Body.String())
		})
	}
}

// mcpBadText is the JSON-RPC error that strictToolArguments answers for a
// tools/call with bad text.
const mcpBadText = `"error":{"code":-32602,"message":"request body is not valid UTF-8 text"}`

func TestBadText_should_be_refused_over_mcp(t *testing.T) {
	s := newProbeServer(t)
	for _, p := range badTextProbes {
		t.Run(p.name, func(t *testing.T) {
			got := s.mcp(p.tool, p.args)

			assert.Contains(t, got, mcpBadText)
		})
	}
}

// Path and query arguments are decoded by huma mcp before the inner request,
// so the check on the raw call is the one that sees them.
func TestBadText_should_be_refused_in_an_mcp_query_or_path_argument(t *testing.T) {
	cases := []struct{ name, tool, args string }{
		{"query lone surrogate", "list_players", `{"external_id":"` + esc("ud800") + `"}`},
		{"query invalid byte", "list_players", "{\"external_id\":\"a\xffb\"}"},
		{"path lone surrogate", "get_leaderboard", `{"leaderboard_id":"` + esc("udc00") + `"}`},
	}
	s := newProbeServer(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := s.mcp(c.tool, c.args)

			assert.Contains(t, got, mcpBadText)
		})
	}
}

// A json.RawMessage body is copied without a decode of its strings, so the
// bytes would reach the store unchanged.
func TestBadText_should_be_refused_in_a_raw_json_body(t *testing.T) {
	cases := []struct{ name, body string }{
		{"invalid byte", "{\"a\":\"x\xffy\"}"},
		{"lone surrogate", `{"a":"` + esc("ud800") + `"}`},
	}
	s := newProbeServer(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := s.do(http.MethodPut, "/v1/storage/objects/slot", c.body)

			assert.Equal(t, []any{http.StatusBadRequest, true}, []any{rec.Code, strings.Contains(rec.Body.String(), errBodyTextJSON)}, rec.Body.String())
		})
	}
}

// pairEsc is the escape text of U+1F600 as a surrogate pair.
var pairEsc = esc("ud83d") + esc("ude00")

// The probe server cannot answer 200: the call passes the text check and is
// refused later by the handler.
func TestGoodText_should_pass_the_text_check_over_mcp(t *testing.T) {
	s := newProbeServer(t)

	got := s.mcp("create_api_key", `{"label":"`+pairEsc+`"}`)

	assert.Equal(t, []any{true, false},
		[]any{strings.Contains(got, `\"status\":401`), strings.Contains(got, `request body is not valid UTF-8 text`)}, got)
}

type echoNameInput struct {
	Body struct {
		Name string `json:"name"`
	}
}

type echoNameOutput struct {
	Body struct {
		Name string `json:"name"`
	}
}

// newEchoServer serves one JSON operation through the shared huma config.
// It answers the decoded name.
func newEchoServer() http.Handler {
	r := chi.NewRouter()
	r.Route("/v1", func(r chi.Router) {
		api := groupAPI(r, newHumaConfig("test"))
		huma.Register(api, huma.Operation{OperationID: "echoName", Method: http.MethodPost, Path: "/v1/echo"},
			func(_ context.Context, in *echoNameInput) (*echoNameOutput, error) {
				out := &echoNameOutput{}
				out.Body.Name = in.Body.Name
				return out, nil
			})
	})
	return r
}

func TestGoodText_should_be_accepted(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"surrogate pair", `{"name":"` + pairEsc + `"}`, "\U0001F600"},
		{"upper-case surrogate pair", `{"name":"` + esc("uD83D") + esc("uDE00") + `"}`, "\U0001F600"},
		{"escaped replacement character", `{"name":"` + esc("ufffd") + `"}`, "\uFFFD"},
		{"literal replacement character", "{\"name\":\"a\xef\xbf\xbdb\"}", "a\uFFFDb"},
		// 87000 pairs of 12 bytes: about 1 MB, under the 1048576 default limit.
		{"large body", `{"name":"` + strings.Repeat(pairEsc, 87000) + `"}`, strings.Repeat("\U0001F600", 87000)},
	}
	h := newEchoServer()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			var got struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			assert.Equal(t, []any{http.StatusOK, c.want}, []any{rec.Code, got.Name})
		})
	}
}
