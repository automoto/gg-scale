package httpapi

import (
	"github.com/automoto/gg-scale/internal/lifecycle"
	"github.com/automoto/gg-scale/internal/realtime"
)

// APIWork is the API server's meaning of work for the lifecycle contract.
// Draining refuses new realtime WebSocket upgrades; plain HTTP requests are
// still served, because the load balancer stops sending them once /readyz
// turns 503. A drain waits on open sockets plus in-flight HTTP requests.
// River and the matchmaker worker are deliberately not part of it: River's
// queue pause is cluster-wide, not per node, and both already stop in the
// normal shutdown path.
type APIWork struct {
	Realtime *realtime.Drain
	Inflight *lifecycle.Inflight
}

// BeginDrain refuses new realtime upgrades.
func (w APIWork) BeginDrain() { w.Realtime.BeginDrain() }

// EndDrain accepts realtime upgrades again.
func (w APIWork) EndDrain() { w.Realtime.EndDrain() }

// ActiveWork reports open realtime sockets plus in-flight HTTP requests.
func (w APIWork) ActiveWork() int64 { return w.Realtime.Count() + w.Inflight.Count() }

// RefusedWork reports realtime upgrades refused while draining.
func (w APIWork) RefusedWork() int64 { return w.Realtime.RefusedWork() }

// Details is the API server's "details" object in the /lifecycle JSON.
func (w APIWork) Details() map[string]any {
	return map[string]any{
		"realtime_connections": w.Realtime.Count(),
		"inflight_requests":    w.Inflight.Count(),
	}
}
