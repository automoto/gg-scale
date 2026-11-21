package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/mcp"

	"github.com/automoto/gg-scale/internal/managementtoken"
)

// mcpMaxBodyBytes limits the body of a POST /mcp request.
const mcpMaxBodyBytes = 131072

// mcpInstructions is sent to MCP clients when they connect.
const mcpInstructions = "ggscale backend for one Game Project. Use project_health_check first " +
	"to find setup problems. All tools act on the Game Project of the token; no tool takes a project ID. " +
	"A tool that you expect is missing when the token lacks its scope: call get_token to see the scopes of the token."

// attachManagementMCP serves the /v1/project operations as MCP tools on
// POST /mcp. api must be bound to a root-level chi group: a tool call sends
// the full /v1/project path back through that group's router.
func attachManagementMCP(api huma.API, version string) {
	mcp.Attach(api, mcp.Options{
		Name:         "ggscale",
		Version:      version,
		Instructions: mcpInstructions,
		Path:         "/mcp",
		Filter:       managementToolFilter,
	})
}

// managementToolFilter offers a tool only when it is a row of projectOps and
// the token has the row's scope, or one of its anyScope list. It is not the
// authorization boundary: the wrapper of each operation checks the scope and
// the creator pair again.
func managementToolFilter(ctx context.Context, op *huma.Operation) bool {
	if _, ok := op.Metadata[managementToolMetadata].(string); !ok {
		return false
	}
	p, ok := managementtoken.FromContext(ctx)
	if !ok {
		return false
	}
	if scope, _ := op.Metadata[managementScopeMetadata].(string); scope != "" {
		return p.HasScope(scope)
	}
	anyScope, _ := op.Metadata[managementAnyScopeMetadata].([]string)
	return len(anyScope) == 0 || slices.ContainsFunc(anyScope, p.HasScope)
}

// managementTool is a registered row operation, the argument names its tool
// accepts, and the names of its path parameters.
type managementTool struct {
	op   *huma.Operation
	args map[string]bool
	path map[string]bool
}

// managementToolArguments returns, for each tool of projectOps registered in
// doc, the names of its path and query parameters and the top-level
// properties of its request body.
func managementToolArguments(doc *huma.OpenAPI) map[string]managementTool {
	out := map[string]managementTool{}
	for _, item := range doc.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if op == nil {
				continue
			}
			tool, ok := op.Metadata[managementToolMetadata].(string)
			if !ok {
				continue
			}
			args := map[string]bool{}
			path := map[string]bool{}
			for _, p := range op.Parameters {
				if p.In == "path" || p.In == "query" {
					args[p.Name] = true
				}
				if p.In == "path" {
					path[p.Name] = true
				}
			}
			if op.RequestBody != nil && op.RequestBody.Content["application/json"] != nil {
				body := op.RequestBody.Content["application/json"].Schema
				if body != nil && body.Ref != "" {
					body = doc.Components.Schemas.SchemaFromRef(body.Ref)
				}
				if body != nil {
					for name := range body.Properties {
						args[name] = true
					}
				}
			}
			out[tool] = managementTool{op: op, args: args, path: path}
		}
	}
	return out
}

// mcpRequest and mcpToolCallParams are the parts of a JSON-RPC tools/call
// request that the argument check reads. They are decoded in the same two
// steps as huma does, so a duplicate "params" key gives the same value here
// and in huma.
type mcpRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpToolCallParams struct {
	Name      string                     `json:"name"`
	Arguments map[string]json.RawMessage `json:"arguments"`
}

// strictToolArguments refuses a tools/call whose tool is a row of projectOps
// and that has an argument the tool does not accept. huma drops such an
// argument, so a misspelled or old setting name would remove that setting.
// The answer is a JSON-RPC invalid-params error, as for an unknown tool. A
// tool the token cannot see is passed on, so it still answers unknown tool.
func strictToolArguments(doc *huma.OpenAPI) func(http.Handler) http.Handler {
	tools := managementToolArguments(doc)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					w.WriteHeader(http.StatusRequestEntityTooLarge)
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			// When a decode fails here, huma's decode of the same bytes also
			// fails, so the request is refused there.
			var req mcpRequest
			if json.Unmarshal(bytes.TrimSpace(body), &req) != nil || req.Method != "tools/call" || len(req.ID) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			var call mcpToolCallParams
			if json.Unmarshal(req.Params, &call) != nil {
				next.ServeHTTP(w, r)
				return
			}
			tool, ok := tools[call.Name]
			if !ok || !managementToolFilter(r.Context(), tool.op) {
				next.ServeHTTP(w, r)
				return
			}
			var unknown, empty []string
			for name, raw := range call.Arguments {
				switch {
				case !tool.args[name]:
					unknown = append(unknown, name)
				case tool.path[name] && emptyPathValue(raw):
					// An empty path argument routes to the collection path,
					// which is a different operation.
					empty = append(empty, name)
				}
			}
			switch {
			case len(unknown) > 0:
				slices.Sort(unknown)
				writeInvalidParams(w, req.ID, "unknown argument for "+call.Name+": "+strings.Join(unknown, ", "))
			case len(empty) > 0:
				slices.Sort(empty)
				writeInvalidParams(w, req.ID, "empty path argument for "+call.Name+": "+strings.Join(empty, ", "))
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// writeInvalidParams answers a JSON-RPC invalid-params error, the same form
// as huma's answer for an unknown tool.
func writeInvalidParams(w http.ResponseWriter, id json.RawMessage, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": -32602, "message": msg},
	})
}

// emptyPathValue reports whether huma builds an empty path segment from raw:
// an empty string, or an array whose values join to an empty string.
func emptyPathValue(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s) == ""
	}
	var items []string
	if json.Unmarshal(raw, &items) == nil {
		return strings.TrimSpace(strings.Join(items, ",")) == ""
	}
	return false
}
