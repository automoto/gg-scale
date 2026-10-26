package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/automoto/gg-scale/internal/relaynode"
)

// platformAdminSecurity is the requirement for the platform-admin API: the
// PLATFORM_ADMIN_TOKEN bearer, never a Game Project key.
var platformAdminSecurity = []map[string][]string{{"PlatformAdminToken": {}}}

// RelayNodeSelector picks the relay URLs for a credential request.
// relaynode.Selector implements it.
type RelayNodeSelector interface {
	Select(ctx context.Context, region string) (relaynode.Selection, error)
}

// RelayNodeAdmin is the relay node registry the admin routes manage.
// relaynode.Store implements it.
type RelayNodeAdmin interface {
	Create(ctx context.Context, spec relaynode.Spec) (relaynode.Node, error)
	List(ctx context.Context) ([]relaynode.Node, error)
	Retire(ctx context.Context, id int64) (relaynode.Node, error)
}

type createRelayNodeInput struct {
	Body struct {
		Region       string   `json:"region" minLength:"1" maxLength:"64" example:"us-east" doc:"Region clients ask for with ?region=. Lowercase letters, digits and dashes."`
		TURNURLs     []string `json:"turn_urls" minItems:"1" maxItems:"16" example:"turn:relay-1.example.com:3478?transport=udp" doc:"turn: or turns: URIs clients dial, in preference order."`
		LifecycleURL string   `json:"lifecycle_url" minLength:"1" maxLength:"2048" example:"http://10.0.0.5:9091/lifecycle" doc:"The node's GET /lifecycle URL, reachable from the API servers."`
	}
}

type relayNodeOutput struct {
	Body relaynode.Node
}

type listRelayNodesOutput struct {
	Body struct {
		Nodes []relaynode.Node `json:"nodes"`
	}
}

type relayNodeIDInput struct {
	ID int64 `path:"id" minimum:"1" example:"7"`
}

func registerAdminRelayNodes(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "createRelayNode",
		Method:        http.MethodPost,
		Path:          "/v1/admin/relay-nodes",
		Summary:       "Register a relay node",
		Description:   "Registers a relay node as known. Credentials list it once a probe sees it active, within one probe interval.",
		Tags:          []string{"Platform Admin"},
		Security:      platformAdminSecurity,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createRelayNodeInput) (*relayNodeOutput, error) {
		n, err := d.RelayNodeAdmin.Create(ctx, relaynode.Spec{
			Region:       in.Body.Region,
			TURNURLs:     in.Body.TURNURLs,
			LifecycleURL: in.Body.LifecycleURL,
		})
		if err != nil {
			return nil, relayNodeError(err)
		}
		return &relayNodeOutput{Body: n}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "listRelayNodes",
		Method:      http.MethodGet,
		Path:        "/v1/admin/relay-nodes",
		Summary:     "List relay nodes",
		Description: "Lists every registered relay node, retired ones included, with its last probe result.",
		Tags:        []string{"Platform Admin"},
		Security:    platformAdminSecurity,
	}, func(ctx context.Context, _ *struct{}) (*listRelayNodesOutput, error) {
		nodes, err := d.RelayNodeAdmin.List(ctx)
		if err != nil {
			return nil, relayNodeError(err)
		}
		out := &listRelayNodesOutput{}
		out.Body.Nodes = nodes
		if out.Body.Nodes == nil {
			out.Body.Nodes = []relaynode.Node{}
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "retireRelayNode",
		Method:      http.MethodPost,
		Path:        "/v1/admin/relay-nodes/{id}/retire",
		Summary:     "Retire a relay node",
		Description: "Stops advertising and probing a node. The row is kept. Retiring a retired node is a no-op.",
		Tags:        []string{"Platform Admin"},
		Security:    platformAdminSecurity,
	}, func(ctx context.Context, in *relayNodeIDInput) (*relayNodeOutput, error) {
		n, err := d.RelayNodeAdmin.Retire(ctx, in.ID)
		if err != nil {
			return nil, relayNodeError(err)
		}
		return &relayNodeOutput{Body: n}, nil
	})
}

func relayNodeError(err error) error {
	switch {
	case errors.Is(err, relaynode.ErrInvalid):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, relaynode.ErrDuplicate):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, relaynode.ErrNotFound):
		return huma.Error404NotFound("relay node not found")
	default:
		return huma.Error500InternalServerError("internal error")
	}
}
