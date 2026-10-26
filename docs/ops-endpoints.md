# Process lifecycle endpoints

Both ggscale processes implement one lifecycle contract: the API server
(`ggscale-server`) and the relay node (`ggscale-server relay`). A deploy tool
or load balancer can take a node out of service, check that it has emptied,
and put it back, without restarting the container. The routes, JSON, env vars,
and token are the same for both processes. The code lives in
`internal/lifecycle`.

## Where the routes are served

| Process | Listener | Next to |
|---|---|---|
| API server | the main `HTTP_ADDR` listener, at root (no `/v1` prefix) | `/metrics` |
| Relay node | `RELAY_HEALTH_ADDR` (off when unset) | `/healthz`, `/metrics` |

The API server serves these routes on its public listener. Keep `/readyz` and
`/lifecycle*` off the public edge the same way you keep `/metrics` off it, for
example with a reverse-proxy rule that only forwards `/v1/*`. The routes are
not in `openapi.yaml`, just like `/metrics`.

## States

```
starting ──▶ active ◀──▶ draining ──▶ stopping
```

| State | Meaning |
|---|---|
| `starting` | The process is booting and is not ready for traffic. |
| `active` | Ready. Accepts new work. |
| `draining` | Refuses new work. Work already in flight continues. Entered by `POST /lifecycle/drain` or by `SIGTERM`. |
| `stopping` | The `SIGTERM` drain wait ended and the process is shutting down. It cannot be resumed. |

State lives in memory only. A restarted process starts in `starting` and
becomes `active` once its listeners are up, even if it was draining before.

## Routes

| Route | Auth | Behaviour |
|---|---|---|
| `GET /readyz` | open | `200` in `active`. `503` in `starting`, `draining` and `stopping`. The body is the state name. |
| `GET /lifecycle` | bearer `LIFECYCLE_TOKEN` when set, else open | The JSON document below. |
| `POST /lifecycle/drain` | bearer `LIFECYCLE_TOKEN`, required | Moves to `draining`. Idempotent: a repeat returns `200` and changes nothing. |
| `POST /lifecycle/resume` | bearer `LIFECYCLE_TOKEN`, required | Moves back to `active`. Idempotent. Returns `409` once the process is `stopping`. |
| `GET /healthz` (relay), `GET /v1/healthz` (API) | open | Liveness only. `200` while the process runs, in every state. |

Both `POST` routes return the same JSON as `GET /lifecycle`.

### Token rule

- `LIFECYCLE_TOKEN` set: send `Authorization: Bearer <token>`. A missing or
  wrong token gets `401`.
- `LIFECYCLE_TOKEN` unset: `GET /lifecycle` is open, the same rule as
  `/metrics`. The two `POST` routes are not registered and return `404`, so an
  unconfigured node cannot be drained by anyone.

The token must be at least 32 bytes, for example `openssl rand -hex 32`. It
supports `LIFECYCLE_TOKEN_FILE` for file-mounted secrets.

### JSON

```json
{
  "role": "relay",
  "state": "draining",
  "commit": "abc1234",
  "started_at": "2026-10-26T10:00:00Z",
  "drain_started_at": "2026-10-26T10:30:00Z",
  "active_work": 12,
  "details": {
    "active_allocations": 12,
    "public_ip": "203.0.113.10",
    "transports": ["udp", "tcp"]
  }
}
```

| Field | Meaning |
|---|---|
| `role` | `relay` or `api`. |
| `state` | One of the states above. |
| `commit` | The build commit. |
| `started_at` | When the process started, UTC. |
| `drain_started_at` | When the current drain began, UTC. `null` while not draining. |
| `active_work` | The work still in flight. A drain is complete when this reaches `0`. |
| `details` | Role-specific numbers, listed below. |

## What each process counts as work

| | Relay node | API server |
|---|---|---|
| Draining refuses | New TURN Allocate requests. | New realtime WebSocket upgrades on `/v1/ws`, with `503` and `Retry-After`. |
| Keeps working | Existing allocations: Refresh, CreatePermission, ChannelBind, and relayed data. | Open WebSockets keep receiving messages. Plain HTTP requests are still served: the load balancer stops sending them once `/readyz` is `503`, and any that still arrive succeed. |
| `active_work` | Live allocations. | Open realtime WebSockets plus in-flight HTTP requests. |
| `details` | `active_allocations`, `public_ip`, `transports` | `realtime_connections`, `inflight_requests` |

On the API server the drain check on `/v1/ws` runs before authentication, so a
refused browser client keeps its one-time ticket and can use it on another
node.

River jobs and the matchmaker worker are not affected by drain. River's queue
pause is cluster-wide state, not per node, and both already stop cleanly in the
normal shutdown path. A relay embedded in the API server, enabled by setting
`RELAY_PUBLIC_IP` on the API host, is not drained either. Run relays as
dedicated `ggscale-server relay` nodes when you need to drain them.

## `SIGTERM`

Both processes handle `SIGTERM` the same way:

1. Enter `draining`, so `/readyz` turns `503` and new work is refused.
2. Wait until `active_work` reaches `0` or `DRAIN_TIMEOUT` elapses, whichever
   comes first. With no work in flight, this step ends immediately.
3. Enter `stopping` and run the existing shutdown path, then exit `0`.

The API server adds one step between 2 and 3: it closes the remaining realtime
WebSockets with close code `1001 Going Away` before `http.Server.Shutdown`
runs, so clients reconnect to another node. The relay node closes its remaining
TURN allocations when the TURN server shuts down.

A second `SIGTERM` during the wait ends the process immediately.

`DRAIN_TIMEOUT` is a Go duration such as `30s` or `2m`. The default is `30s`.

### Container stop timeout

Set the container stop timeout above `DRAIN_TIMEOUT`, with room for the
shutdown path that follows. Otherwise the runtime kills the process in the
middle of the drain. For example, with the default `DRAIN_TIMEOUT=30s`:

```yaml
# docker compose
services:
  server:
    stop_grace_period: 45s
```

```yaml
# Kubernetes pod spec
terminationGracePeriodSeconds: 45
```

## Metrics

Both processes register these on their existing `/metrics`:

| Metric | Type | Meaning |
|---|---|---|
| `ggscale_lifecycle_state{state="..."}` | gauge | `1` for the current state, `0` for the other three. |
| `ggscale_lifecycle_work_refused_total` | counter | New work refused while draining: TURN Allocate requests on the relay, WebSocket upgrades on the API server. |

## Rolling a node

```sh
TOKEN=...                       # LIFECYCLE_TOKEN
NODE=http://10.0.0.5:9091       # relay: RELAY_HEALTH_ADDR. API: the main listener.

# 1. Drain. /readyz turns 503 and the load balancer stops routing here.
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" "$NODE/lifecycle/drain"

# 2. Wait until active_work reaches 0, or give up after your own deadline.
curl -fsS -H "Authorization: Bearer $TOKEN" "$NODE/lifecycle" | jq .active_work

# 3a. Replace the node: SIGTERM finishes the drain and exits 0.
# 3b. Or put it back into service without a restart:
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" "$NODE/lifecycle/resume"
```

For relay nodes, see also [relay-ops.md](relay-ops.md).
