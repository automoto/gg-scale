// Package plugin glues the fleet.Backend contract to hashicorp/go-plugin's
// subprocess+gRPC model. grpc.go is the only translation point between
// package fleet types and protobuf messages.
package plugin

import (
	"context"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/ggscale/ggscale/internal/fleet"
	fleetpb "github.com/ggscale/ggscale/internal/fleet/plugin/proto"
)

// Handshake is the magic-cookie/protocol pair both host and plugin must agree
// on before any RPC is dispatched. ProtocolVersion bumps alongside the proto
// package version on any breaking change.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "GGSCALE_FLEET_PLUGIN",
	MagicCookieValue: "ggscale-v1",
}

// PluginName is the dispense key shared by host and plugin. A single binary
// serves exactly one backend, so the map only ever has one entry.
const PluginName = "fleet_backend"

// Plugins returns the goplugin.PluginMap. The plugin side passes its real
// fleet.Backend in impl; the host passes nil and receives a gRPC client
// wrapper at Dispense time.
func Plugins(impl fleet.Backend) map[string]goplugin.Plugin {
	return map[string]goplugin.Plugin{
		PluginName: &FleetPlugin{Impl: impl},
	}
}

// FleetPlugin embeds goplugin.Plugin so the unused net/rpc stubs are
// satisfied; the transport is gRPC only.
type FleetPlugin struct {
	goplugin.Plugin
	Impl fleet.Backend // populated only on the plugin (serving) side
}

// GRPCServer registers the plugin's fleet.Backend on s. Called by go-plugin.
func (p *FleetPlugin) GRPCServer(_ *goplugin.GRPCBroker, s *grpc.Server) error {
	fleetpb.RegisterFleetBackendServer(s, newGRPCServer(p.Impl))
	return nil
}

// GRPCClient returns the host-side fleet.Backend wrapper around the plugin's
// gRPC client. Called by go-plugin after the subprocess is up.
func (p *FleetPlugin) GRPCClient(ctx context.Context, _ *goplugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return newGRPCClient(ctx, fleetpb.NewFleetBackendClient(c)), nil
}
