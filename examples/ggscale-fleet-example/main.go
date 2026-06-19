// Command ggscale-fleet-example is the reference fleet plugin: a noop backend
// that succeeds instantly with a hard-coded address. It doubles as the
// third-party plugin author template and as the M4.4 integration-test
// fixture.
//
// Invoke: drop the built binary at $GGSCALE_PLUGIN_DIR/ggscale-fleet-example
// and run core with FLEET_BACKEND=plugin:example.
package main

import (
	goplugin "github.com/hashicorp/go-plugin"

	fleetplugin "github.com/ggscale/ggscale/internal/fleet/plugin"
)

func main() {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: fleetplugin.Handshake,
		Plugins:         fleetplugin.Plugins(newNoopBackend()),
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}
