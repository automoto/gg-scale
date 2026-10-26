# Relay node operations

A relay node is a dedicated host running `ggscale-server relay`. It runs only
the TURN listener. It has no database, HTTP API or matchmaker, and it never
mints credentials: the API servers issue them, and the relay verifies them
with the shared secret. All settings are in the relay section of
`.env.example`.

## Minimal configuration

```sh
RELAY_PUBLIC_IP=203.0.113.10        # IPv4 address clients reach
RELAY_SHARED_SECRET=...             # >= 32 bytes, same value as the API servers
RELAY_HEALTH_ADDR=:9091             # /healthz, /metrics, lifecycle routes
LIFECYCLE_TOKEN=...                 # >= 32 bytes, enables drain and resume
```

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

## Draining and replacing a node

A draining relay refuses new TURN Allocate requests. Allocations it already
holds keep refreshing and relaying data, so calls in progress are not cut. The
full contract is in [ops-endpoints.md](ops-endpoints.md).

1. Take the node out of the relay URLs that API servers hand out, such as
   `RELAY_URLS` or DNS, so new credentials point elsewhere.
2. `POST /lifecycle/drain` on `RELAY_HEALTH_ADDR`.
3. Watch `active_work` in `GET /lifecycle` fall as sessions end.
4. Stop the container. `SIGTERM` waits up to `DRAIN_TIMEOUT` for the remaining
   allocations, then closes them and exits `0`. Set the container stop timeout
   above `DRAIN_TIMEOUT`.

To cancel instead, `POST /lifecycle/resume` puts the node back into service
without a restart.

## Rotating the shared secret

The relay accepts every configured secret and the API servers sign with
`RELAY_SHARED_SECRET`, so a rotation needs no downtime:

1. Set the new value as `RELAY_SHARED_SECRET_NEXT` on every relay node and API
   server.
2. Promote it to `RELAY_SHARED_SECRET` everywhere, keeping the old value in
   `RELAY_SHARED_SECRET_NEXT`.
3. After one `RELAY_CRED_TTL`, clear `RELAY_SHARED_SECRET_NEXT`.

Credentials issued before each step stay valid until they expire.
