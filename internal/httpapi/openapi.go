package httpapi

import (
	"reflect"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
)

// OpenAPIDoc builds the /v1 OpenAPI document by registering every operation
// into one shared doc. It needs no live dependencies: the handler closures are
// registered but never invoked, so a zero Deps suffices.
//
// The register* list below MUST mirror NewRouter's registrations. NewRouter
// spreads them across middleware-scoped chi groups (for auth/scope binding);
// here they collapse onto one adapter because the document only needs the
// operation metadata, not the middleware. TestOpenAPIDoc_covers_expected_paths
// guards against drift.
func OpenAPIDoc(version string) *huma.OpenAPI {
	cfg := newHumaConfig(version)
	api := groupAPI(chi.NewRouter(), cfg)
	var d Deps

	registerHealthz(api, d)
	registerAuthRoutes(api, d)
	registerPlayerSessionVerify(api, d)
	registerServerRemoteAddr(api, d)
	registerFleetHeartbeat(api, d)
	registerFleetServersList(api, d)
	registerRelay(api, d)
	registerPresence(api, d)
	registerGameInvites(api, d)
	registerProfileRoutes(api, d)
	registerStorageRoutes(api, d)
	registerLeaderboardReadRoutes(api, d)
	registerLeaderboardSubmit(api, d)
	registerFriendRoutes(api, d)
	registerRemoteAddrRoutes(api, d)
	registerGameSessionRoutes(api, d)
	registerMatchmakerRoutes(api, d)

	doc := cfg.OpenAPI
	enrichVerifyOp(doc)
	addWebSocketStub(doc)
	return doc
}

// enrichVerifyOp fills in the request/response schemas for the verify
// operation. Its handler is a body-callback (to keep the opaque-401 wire), so
// huma emits no schema for it on its own.
func enrichVerifyOp(doc *huma.OpenAPI) {
	item := doc.Paths["/v1/server/player-sessions/verify"]
	if item == nil || item.Post == nil {
		return
	}
	reg := doc.Components.Schemas
	reqSchema := reg.Schema(reflect.TypeOf(playerVerifyRequest{}), true, "PlayerVerifyRequest")
	respSchema := reg.Schema(reflect.TypeOf(playerVerifyResponse{}), true, "PlayerVerifyResponse")
	item.Post.RequestBody = &huma.RequestBody{
		Required: true,
		Content:  map[string]*huma.MediaType{"application/json": {Schema: reqSchema}},
	}
	item.Post.Responses = map[string]*huma.Response{
		"200": {
			Description: "OK",
			Content:     map[string]*huma.MediaType{"application/json": {Schema: respSchema}},
		},
		"401": {
			Description: "Invalid session (opaque; covers every failure mode)",
			Content: map[string]*huma.MediaType{"application/json": {Schema: &huma.Schema{
				Type:       huma.TypeObject,
				Properties: map[string]*huma.Schema{"error": {Type: huma.TypeString}},
			}}},
		},
	}
}

// addWebSocketStub hand-adds the realtime WebSocket route, which stays a plain
// chi handler (not a huma operation) and so is invisible to the generator.
func addWebSocketStub(doc *huma.OpenAPI) {
	doc.Paths["/v1/ws"] = &huma.PathItem{
		Get: &huma.Operation{
			OperationID: "realtimeWebSocket",
			Summary:     "Realtime WebSocket channel",
			Description: "Upgrades to a WebSocket for realtime player events; not a JSON endpoint. " +
				"Authenticate with the tenant API key and the player session token.",
			Tags:     []string{"/v1"},
			Security: playerSecurity,
			Responses: map[string]*huma.Response{
				"101": {Description: "Switching Protocols (WebSocket upgrade)"},
			},
		},
	}
}
