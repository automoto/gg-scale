# Realtime capacity and admission limits

This document describes the production admission controls for the ggscale
player-services WebSocket and HTTP API. These limits protect shared services and
prevent surprise overage charges. They are not dedicated-game-server capacity
and are not, by themselves, performance guarantees.

## What a realtime connection means

A realtime connection is one authenticated persistent connection to `/v1/ws`.
It is used for backend events such as presence, invitations, and matchmaking
results. It does not count:

- a player who is signed in without an open ggscale WebSocket;
- a player connected only to a dedicated game server;
- a game-server process, relay allocation, or fleet slot.

Every persistent connection consumes one slot. A player with multiple
simultaneous connections consumes multiple slots. The separate per-player guard
defaults to four connections.

## Scope

Managed production enforces one connection envelope for each `(tenant,
APP_REGION)` pair. All application processes in the same service region share
that regional envelope through leased PostgreSQL grants. All projects belonging
to the tenant share it. A deployment in another service region has an
independent envelope.

The public plan figure therefore means sustained realtime connections per
tenant, per ggscale service region. It is not a sum of dedicated-server players
and it is not a single worldwide peak.

## Current production defaults

| Service class | Public plan | Sustained connections | Temporary maximum | API rate per key and region | API burst bucket per key and region |
| --- | --- | ---: | ---: | ---: | ---: |
| `tier_0` | Free | 2,500 | 5,000 | 250/sec | 2,500 requests |
| `tier_1` | Pro | 10,000 | 20,000 | 1,000/sec | 10,000 requests |
| `tier_2` | Studio | 50,000 | 100,000 | 5,000/sec | 50,000 requests |
| `tier_3` | Enterprise default | 100,000 | 200,000 | 10,000/sec | 100,000 requests |

Enterprise values are starting defaults. A signed order form and production
capacity review may define different limits.

## Connection burst behavior

Connections at or below the sustained limit are admitted without consuming the
burst budget. Connections between the sustained limit and the temporary maximum
are admitted only while burst budget remains. The temporary maximum is a hard
admission wall.

Each regional envelope begins with 10 minutes of full-2× burst budget. While
usage is above sustained capacity, the budget drains in proportion to the
amount above the sustained limit:

`drain rate = (current connections - sustained connections) / sustained connections`

For Studio's 50,000 sustained limit, a fully replenished budget behaves
as follows:

| Connections in one service region | Approximate time until exhaustion |
| ---: | ---: |
| 50,000 or fewer | Does not drain |
| 60,000 | 50 minutes |
| 75,000 | 20 minutes |
| 80,000 | 16 minutes 40 seconds |
| 100,000 | 10 minutes |

At or below the sustained limit, the budget refills linearly and takes one hour
to refill from empty. Established WebSockets are never closed merely to repair
an exhausted budget or a reduced limit. New connections above the currently
available envelope are rejected before upgrade with HTTP 503 and
`Retry-After: 5`.

## HTTP API limits

The HTTP API uses a token bucket per API key and service region, not one bucket
for the entire tenant. The advertised rate is the refill rate; the burst value
is the maximum number of tokens the bucket can hold. A full bucket does not
mean the platform guarantees that many simultaneous requests.

Publishable-key token routes also have a per-tenant, per-source-IP guard set to
one tenth of the tier's per-key rate and burst. Secret keys bypass that IP guard
but remain subject to their per-key bucket. Password signup and login have a
separate guard of 10 attempts per minute per source IP.

The default API refill rate is sized for one backend action per connected player
every 10 seconds. The default bucket holds one immediate action per sustained
connection, so a full Studio API key can admit a 50,000-request startup or
reconnect wave before settling to 5,000 requests/second. Each API key receives
its own bucket in each region. Platform admins may set a higher tenant API rate
and burst; the override is applied independently to every API key belonging to
that tenant in each region.

A large bucket is admission headroom, not an endpoint-throughput guarantee.
Games with heavier writes or synchronized startup calls must load test their
request mix and use retry, backoff, jitter, caching, and batching where
appropriate.

## Managed and self-hosted behavior

Managed production uses regional leased grants so multiple application
processes share an envelope. Grants renew every 15 seconds and expire after 45
seconds. A bounded process-local emergency allowance handles a short grant-store
failure; it is not additional advertised capacity.

HTTP API token buckets are process-local. Managed production currently runs one
web process in each service region, making the process bucket the regional
bucket. Do not increase the web-process count without first adding a shared
regional API limiter or explicitly dividing the rate and burst across
processes; otherwise every process can spend the full advertised bucket.

Platform admins can persist a tenant-specific sustained and temporary
connection envelope for contracted launches or breakout traffic. The override
is audited, cached for five seconds per application process, and applied per
service region. It changes new admission capacity without closing established
connections. The temporary maximum must be at least the sustained limit.

For self-hosted deployments, `REALTIME_MAX_PER_TENANT` may set one fixed hard
cap for all tenants in that deployment. A positive value disables the
tier-derived burst envelope. Zero uses the service-class defaults.

## Operational interpretation

- Treat the values above as admission limits until a representative load test
  validates the deployed topology and game workload.
- Monitor rejections by reason, grant synchronization errors, emergency
  admissions, open connections, API throttling, database saturation, and
  realtime delivery failures.
- Arrange custom capacity before a planned launch. The default 10-minute burst
  is intended for brief spikes, not an entire launch event.
- Raise tenant-specific API and connection overrides before a planned launch
  when the verified workload needs more than the plan default.

## Implementation references

- `internal/ratelimit/connection_cap.go`: tier connection envelopes.
- `internal/cache/burst.go`: burst drain and refill model.
- `internal/ratelimit/postgres_connection_cap.go`: regional shared grants.
- `internal/realtime/server.go`: WebSocket admission and retry response.
- `internal/ratelimit/tier.go`: API rate and burst defaults.
- `internal/ratelimit/middleware.go`: per-API-key bucket scope.
- `internal/ratelimit/connection_overrides.go`: tenant-specific launch capacity.
- `internal/ratelimit/ip_middleware.go`: authentication and token-route IP guards.
