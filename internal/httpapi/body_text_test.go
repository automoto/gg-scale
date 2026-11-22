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

func TestJSONFormat_should_check_the_text_of_the_body(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"invalid byte", "\"a\xffb\"", errBodyText},
		{"utf-8 encoded surrogate", "\"\xed\xa0\x80\"", errBodyText},
		{"high surrogate alone", `"\ud800"`, errBodyText},
		{"low surrogate alone", `"\udc00"`, errBodyText},
		{"upper-case high surrogate alone", `"\uD800"`, errBodyText},
		{"high surrogate then text", `"\ud800x"`, errBodyText},
		{"high surrogate then other escape", `"\ud800\n"`, errBodyText},
		{"high high low", `"\ud800𐀀"`, errBodyText},
		{"low then high", `"\udc00\ud800"`, errBodyText},
		{"surrogate in a key", `{"\ud800":1}`, errBodyText},
		{"high surrogate at the end of the body", `\ud800`, errBodyText},
		{"surrogate pair", `"😀"`, nil},
		{"upper-case surrogate pair", `"😀"`, nil},
		{"literal replacement character", "\"\xef\xbf\xbd\"", nil},
		{"escaped replacement character", `"�"`, nil},
		{"escaped backslash before u", `"\\ud800"`, nil},
		{"four-byte character", `"😀"`, nil},
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
	{"key label high surrogate", "create_api_key", http.MethodPost, "/v1/project/api-keys", `{"label":"\ud800"}`},
	{"key label low surrogate", "create_api_key", http.MethodPost, "/v1/project/api-keys", `{"label":"\udc00"}`},
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

func TestBadText_should_be_refused_over_mcp(t *testing.T) {
	s := newProbeServer(t)
	for _, p := range badTextProbes {
		t.Run(p.name, func(t *testing.T) {
			got := s.mcp(p.tool, p.args)

			assert.Equal(t, []any{true, true, false},
				[]any{strings.Contains(got, `\"status\":400`), strings.Contains(got, `request body is not valid UTF-8 text`), strings.Contains(got, `\"value\"`)},
				got)
		})
	}
}

// The probe server cannot answer 200: the call passes the text check and is
// refused later by the handler.
func TestGoodText_should_pass_the_text_check_over_mcp(t *testing.T) {
	s := newProbeServer(t)

	got := s.mcp("create_api_key", `{"label":"😀"}`)

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
		{"surrogate pair", `{"name":"😀"}`, "😀"},
		{"literal replacement character", "{\"name\":\"a\xef\xbf\xbdb\"}", "a�b"},
		{"large body", `{"name":"` + strings.Repeat(`é😀`, 70000) + `"}`, strings.Repeat("é😀", 70000)},
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
