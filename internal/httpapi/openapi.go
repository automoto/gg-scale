package httpapi

import (
	"reflect"
	"strings"

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
	registerAuthPasswordRoutes(api, d)
	registerAuthTokenRoutes(api, d)
	registerAuthLinkRoutes(api, d)
	registerAuthAccountRoutes(api, d)
	registerRemoteConfig(api, d)
	registerPlayerSessionVerify(api, d)
	registerServerRemoteAddr(api, d)
	registerServerLeaderboardSubmit(api, d)
	registerServerStorageRoutes(api, d)
	registerFleetHeartbeat(api, d)
	registerFleetServersList(api, d)
	registerRelay(api, d)
	registerPresence(api, d)
	registerGameInvites(api, d)
	registerProfileRoutes(api, d)
	registerPlayerLookupRoutes(api, d)
	registerFriendCodeRoutes(api, d)
	registerStorageRoutes(api, d)
	registerLeaderboardReadRoutes(api, d)
	registerLeaderboardDiscoveryRoutes(api, d)
	registerLeaderboardPeriodRoutes(api, d)
	registerLeaderboardSubmit(api, d)
	registerFriendRoutes(api, d)
	registerRemoteAddrRoutes(api, d)
	registerGameSessionRoutes(api, d)
	registerMatchmakerRoutes(api, d)
	registerPartyRoutes(api, d)
	registerRealtimeTicket(api, d)

	doc := cfg.OpenAPI
	enrichVerifyOp(doc)
	addWebSocketStub(doc)
	noteSecretKeyOperations(doc)
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
				"Authenticate with your Game Project's publishable API key (the key the player logged in " +
				"with) and the player session token in headers, or, " +
				"from a browser (which cannot set WebSocket headers), with a one-time ticket from " +
				"POST /v1/ws/ticket in the ticket query parameter and no headers.\n\n" +
				"The server sends JSON text frames of the form {\"type\": ..., \"payload\": {...}}. Events are " +
				"best effort: a client that misses one recovers the state with the matching GET.\n\n" +
				"- matchmaker_matched: a ticket matched. Payload: ticket_id, match_id, mode, address, " +
				"protocol_hint, session_id, join_code, host_player_id, users.\n" +
				"- presence: a friend changed status. Payload: player_id, status, session_id.\n" +
				"- game_invite: a friend invited you to a game session. Payload: invite_id, session_id, join_code.\n" +
				"- party_changed: a party you are in, or were removed from, changed. Payload: party_id, version, " +
				"state. Call GET /v1/parties/{id} when version is newer than yours; 404 means you are no longer a member.\n" +
				"- party_invite: a friend invited you to a party. Payload: invite_id, party_id, from_player_id.",
			Tags:     []string{"Realtime"},
			Security: append([]map[string][]string{{}}, playerSecurity...),
			Parameters: []*huma.Param{{
				Name:        "ticket",
				In:          "query",
				Description: "One-time ticket from POST /v1/ws/ticket. Use it instead of the auth headers.",
				Schema:      &huma.Schema{Type: huma.TypeString},
			}},
			Responses: map[string]*huma.Response{
				"101": {Description: "Switching Protocols (WebSocket upgrade)"},
			},
		},
	}
}

// serverOnlyNote starts the description of each operation that refuses a
// publishable key.
const serverOnlyNote = "Requires a secret API key: call it from a game server or backend, never from a game client."

// noteSecretKeyOperations starts the description of each operation that
// accepts only the SecretKey scheme with serverOnlyNote, for readers who skip
// the security section.
func noteSecretKeyOperations(doc *huma.OpenAPI) {
	for _, item := range doc.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Put, item.Post, item.Delete, item.Patch} {
			if op == nil || !secretKeyOnly(op) {
				continue
			}
			op.Description = strings.TrimSpace(serverOnlyNote + " " + op.Description)
		}
	}
}

func secretKeyOnly(op *huma.Operation) bool {
	if len(op.Security) == 0 {
		return false
	}
	for _, req := range op.Security {
		if _, ok := req["SecretKey"]; !ok {
			return false
		}
	}
	return true
}
