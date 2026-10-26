# Relay node operations

A relay node is a dedicated host running `ggscale-server relay`. It runs only
the TURN listener. It has no database, HTTP API or matchmaker, and it never
mints credentials: the API servers issue them, and the relay verifies them
with the shared secret. All settings are in the relay section of
`.env.example`.

The API servers keep a registry of relay nodes. A platform admin registers and
retires nodes through the admin API, a probe reads each node's
`GET /lifecycle` every 10 seconds, and relay credentials list only the nodes
that are up and `active`. Adding, replacing or rolling back a node needs no
API-server restart.

## Minimal configuration

```sh
RELAY_PUBLIC_IP=203.0.113.10        # IPv4 address clients reach
RELAY_SHARED_SECRET=...             # >= 32 bytes, same value as the API servers
RELAY_HEALTH_ADDR=:9091             # /healthz, /metrics, lifecycle routes
LIFECYCLE_TOKEN=...                 # >= 32 bytes, enables drain and resume
```

On the API servers:

```sh
PLATFORM_ADMIN_TOKEN=...            # >= 32 bytes, enables /v1/admin/relay-nodes
LIFECYCLE_TOKEN=...                 # the same value as the relay nodes
```

`RELAY_HEALTH_ADDR` must be reachable from the API servers, because the probe
reads `GET /lifecycle` there. The probe sends the API server's
`LIFECYCLE_TOKEN`, so one value across the fleet is enough. To give relay nodes
their own `LIFECYCLE_TOKEN`, set `RELAY_PROBE_TOKEN` on the API servers to it.
[operator-tokens.md](operator-tokens.md) lists every token and what it guards.

## Ports

| Port | Protocol | Setting | Default |
|---|---|---|---|
| TURN | UDP | `RELAY_UDP_PORT` | `3478` |
| TURN over TCP | TCP | `RELAY_TCP_PORT` | off |
| TURNS (TLS) | TCP | `RELAY_TLS_PORT`, with `RELAY_TLS_CERT_FILE` and `RELAY_TLS_KEY_FILE` | off |
| Relayed media | UDP | `RELAY_MIN_PORT` to `RELAY_MAX_PORT` | OS ephemeral range |
| Health, metrics, lifecycle | TCP | `RELAY_HEALTH_ADDR` | off |

Set a media port range so the firewall opens only that range. Keep
`RELAY_MAX_ALLOCATIONS` at or below the width of the range. Keep
`RELAY_HEALTH_ADDR` on a private network such as a tailnet, never on the
public interface.

## Health and readiness

| Route | Use |
|---|---|
| `GET /healthz` | Liveness. `200` while the process runs, including while draining. |
| `GET /readyz` | Readiness. `200` only when the node takes new allocations. |
| `GET /lifecycle` | State, live allocations, public IP and transports, as JSON. |
| `GET /metrics` | Prometheus. Behind `RELAY_METRICS_TOKEN` when set. |

## Metrics to alert on

| Metric | Watch for |
|---|---|
| `ggscale_relay_up` | Scrape target down. |
| `ggscale_relay_active_allocations` | Approaching `RELAY_MAX_ALLOCATIONS` or the media port range width. |
| `ggscale_relay_allocations_rejected_total` | Allocations refused by the node cap. |
| `ggscale_relay_auth_failures_total` | Credential probing, or a secret mismatch with the API servers. |
| `ggscale_relay_alloc_throttled_total` | One credential hitting the per-player allocation limit. |
| `ggscale_relay_peer_rejected_total` | Attempts to relay into private address space. |
| `ggscale_lifecycle_state` | A node left in `draining` after a deploy. |
| `ggscale_lifecycle_work_refused_total` | Clients still sent to a draining node. |
| `ggscale_relay_no_eligible_nodes_total` | On the API servers: credential requests refused because no relay node was eligible. |

## Relay node registry

### Which nodes credentials list

A node is **eligible** when all of these hold:

- it is registered and not retired;
- its last probe got a `2xx` with `role: "relay"` and `state: "active"`;
- that probe ran less than 30 seconds (three probe intervals) ago.

`POST /v1/relay/credentials?region=<name>` lists the `turn_urls` of every
eligible node in that region (matched case-insensitively), in shuffled node order. Each node's own URLs keep
the order they were registered in. When the region has no eligible node, or the
client sends no region, the response lists every eligible node instead. The
response's `region` field names the region served, or is `""` when the nodes
span regions. With no eligible node anywhere, the request fails with `503` and
`Retry-After: 10`, and `ggscale_relay_no_eligible_nodes_total` counts it.

A node that stops answering, answers non-`2xx`, reports another `role`, or
reports `draining` stops being listed after its next probe. A node whose probes
stop entirely drops out once its last good probe is 30 seconds old. A `401`
means the probe token (`RELAY_PROBE_TOKEN`, or the API server's
`LIFECYCLE_TOKEN` when that is unset) does not match the node's
`LIFECYCLE_TOKEN`. The API
server logs that once per node, not on every probe.

The probe runs as the River job `relay_node_probe` on its own queue, once
across the API-server cluster, so slow jobs on River's default queue cannot
delay it. If River fails to start, each API server logs a warning and probes
the nodes itself every 10 seconds instead. The admin list shows why a node's
last probe failed in `last_probe_error`, for example `timeout`, `status 503`
or `unauthorized`.

A change on a node reaches credentials in two steps. First a probe has to
record it: the next probe starts within 10 seconds and can take up to 3
seconds to time out. Then each API server keeps using the eligible set it last
read for up to 2 seconds. Under normal scheduling that is about 15 seconds in
total, and any delay in running the probe job comes on top. Rather than waiting
a fixed time, watch the node in the admin list: once its `eligible` flag and
`last_probe_at` show the change, wait 2 more seconds.

### Admin API

The routes are mounted only when `PLATFORM_ADMIN_TOKEN` is set, and they take
it as a bearer token. They are in `openapi.yaml` under **Platform Admin**.

```sh
API=https://api.example.com
ADMIN=...                           # PLATFORM_ADMIN_TOKEN

# Register a node. It is listed once a probe sees it active.
curl -fsS -X POST -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  "$API/v1/admin/relay-nodes" -d '{
    "region": "us-east",
    "turn_urls": [
      "turn:relay-2.example.com:3478?transport=udp",
      "turns:relay-2.example.com:5349?transport=tcp"
    ],
    "lifecycle_url": "http://10.0.0.6:9091/lifecycle"
  }'

# List nodes with their last probe and whether they are eligible.
curl -fsS -H "Authorization: Bearer $ADMIN" "$API/v1/admin/relay-nodes"

# Retire a node: never listed or probed again. The row is kept.
curl -fsS -X POST -H "Authorization: Bearer $ADMIN" "$API/v1/admin/relay-nodes/7/retire"
```

A region is lowercase letters, digits and dashes. Only one registered node may
use a given `lifecycle_url`; a retired node's URL can be registered again.
`lifecycle_url` may point at a loopback or private address. Link-local
addresses (including `169.254.169.254`), cloud metadata addresses, multicast
and unspecified addresses are refused when a node is registered and again when
the probe connects, after DNS resolution. The probe ignores proxy settings and
does not follow redirects.

### The relay embedded in an API server

Setting `RELAY_PUBLIC_IP` on an API server runs a TURN relay inside it. That
server lists its own relay, `turn:<RELAY_PUBLIC_IP>:<RELAY_UDP_PORT>?transport=udp`.
It has no region, so it is listed only when a response falls back to every
eligible node, which includes the case where no registered node is eligible,
and when the registry cannot be read. A single-host install therefore needs no
registration. When clients reach it at another port or through a hostname, for
example because the UDP port is published on a different host port, list the
URIs to advertise in `RELAY_EMBEDDED_URLS`. It is not
probed and cannot be drained. Do not register it as a node: its
`GET /lifecycle` reports `role: "api"`, so the probe marks it unhealthy. To
stop listing it, unset `RELAY_PUBLIC_IP` on that API server.

## Replacing a node (blue/green)

A draining relay refuses new TURN Allocate requests. Allocations it already
holds keep refreshing and relaying data, so calls in progress are not cut. The
full contract is in [ops-endpoints.md](ops-endpoints.md).

```sh
NEW=http://10.0.0.6:9091            # new node's RELAY_HEALTH_ADDR
OLD=http://10.0.0.5:9091            # old node's RELAY_HEALTH_ADDR
TOKEN=...                           # LIFECYCLE_TOKEN
```

1. **Register** the new node through the admin API, as above.
2. **Wait for the new node to be ready**: `GET $NEW/readyz` returns `200`, and
   the node shows `"eligible": true` in `GET /v1/admin/relay-nodes`. Then wait
   2 seconds for the API servers' cached eligible set.
3. **Drain the old node**: `POST $OLD/lifecycle/drain`. Wait until the admin
   list shows it with `"eligible": false` and `last_lifecycle_state`
   `draining`, then 2 more seconds. From then on new credentials do not list
   it.
4. **Wait for `active_allocations` to reach `0`** in `GET $OLD/lifecycle`
   (`.details.active_allocations`). Credentials issued before the drain still
   list the old node until they expire after `RELAY_CRED_TTL`. The draining
   node refuses new allocations from them, and the client's next credential
   request lists the new node.
5. **Retire the old node**: `POST /v1/admin/relay-nodes/<old id>/retire`. Then
   stop it. With nothing left to drain, `SIGTERM` exits at once.

```sh
curl -fsS "$NEW/readyz"
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" "$OLD/lifecycle/drain"
curl -fsS -H "Authorization: Bearer $TOKEN" "$OLD/lifecycle" | jq .details.active_allocations
```

**Rollback**, at any point before step 5: `POST $OLD/lifecycle/resume` puts
the old node back into service, and `POST $NEW/lifecycle/drain` takes the new
one out. Each change reaches credentials once a probe records it, as above. Retire the new node
once it reports `active_allocations` of `0`. After step 5, register the old
node again instead of resuming it.

### Stop timeout

`DRAIN_TIMEOUT` defaults to `0s` on a relay node, because the sequence above
drains the node before it is stopped. An undrained relay that gets `SIGTERM`
closes its allocations and exits at once, well within Docker's default 10s
stop timeout. If you set a longer `DRAIN_TIMEOUT`, set the container stop
timeout above it.

## Upgrading from `RELAY_URLS`

`RELAY_URLS` is removed. An API server with it set refuses to start and says
so. To upgrade:

1. Set `PLATFORM_ADMIN_TOKEN` on the API servers and remove `RELAY_URLS`.
   Give each relay node a `RELAY_HEALTH_ADDR` the API servers can reach, and
   the same `LIFECYCLE_TOKEN` as the API servers (or set `RELAY_PROBE_TOKEN`
   on the API servers to the relay nodes' token).
2. Deploy. Until a node is registered and probed, relay credential requests
   get `503` unless an API server runs an embedded relay.
3. Register every relay node that used to be in `RELAY_URLS`.

`RELAY_STUN_URLS` is unchanged.

## Rotating the shared secret

The relay accepts every configured secret and the API servers sign with
`RELAY_SHARED_SECRET`, so a rotation needs no downtime:

1. Set the new value as `RELAY_SHARED_SECRET_NEXT` on every relay node and API
   server.
2. Promote it to `RELAY_SHARED_SECRET` everywhere, keeping the old value in
   `RELAY_SHARED_SECRET_NEXT`.
3. After one `RELAY_CRED_TTL`, clear `RELAY_SHARED_SECRET_NEXT`.

Credentials issued before each step stay valid until they expire.
