# Cache improvement contingency design

Status: **deferred contingency; not approved for implementation**

Last reviewed: 2026-07-22

This document describes an optional shared regional rate-limiting architecture
for a future ggscale deployment that has outgrown process-local request
buckets. It does not change the current decision in
`docs/in-memory-cache-rollout.md`: ggscale uses a process-local sharded cache,
keeps PostgreSQL as the source of truth, and does not add a separate cache
service preemptively.

Olric has been removed from the current architecture and is not a candidate in
this design. This document supersedes neither the in-memory-cache rollout nor
its remaining production gates.

## Decision

Shelve the shared limiter as a prepared contingency, not a next step.

There is no measured need for it today:

- Production currently has one app host per region. With one serving process
  in a region, its process-local HTTP and invite buckets are already exact for
  that region.
- Even with two app processes, the accepted worst case is approximately two
  times a configured bucket when traffic is spread evenly. The buckets are
  abuse guardrails, not billing or entitlement counters.
- The in-memory-cache rollout is still `NO-GO` pending its final production
  gates. Adding a new deployment unit before that decision is fully landed
  would mix two infrastructure changes without evidence that the second is
  required.
- A remote design adds a network decision to every protected request. Player
  routes can require two serialized decisions, first for the API key and then
  for the authenticated player, while also adding a service that must be
  monitored, versioned, and deployed compatibly with the app.
- A safe remote-limiter failure mode falls back to process-local enforcement.
  Therefore the system must remain safe under local semantics anyway; a remote
  service tightens the healthy common case but does not remove that worst case.

The immediate cache-related work is to complete the current rollout. The
invite partial-debit defect described below is a separate local correctness
fix and does not justify a distributed service.

## Current behavior and verified integration points

The proposed integration points match the current code:

- `internal/httpapi/router.go` installs `tenant.New`, then the API-key rate
  limiter, then (for authenticated player routes) `playerauth.New` and the
  player limiter. A gateway in front of the app cannot reproduce those
  identities without duplicating authentication and database behavior.
- `internal/ratelimit/middleware.go` exposes small `Limiter` and `Refunder`
  interfaces. A remote client can implement them without changing handlers or
  the public HTTP response contract.
- `internal/ratelimit/overrides.go` caches rate-limit overrides for five
  seconds. Keeping policy resolution in the app initially preserves that
  consistency model.
- `internal/ratelimit/tier.go` gives `tier_0` a sustained rate of 250 requests
  per second.
- `internal/ratelimit/ip_middleware.go` currently includes the raw client IP in
  an in-memory bucket key. A remote store would require a stable keyed digest
  instead so it does not receive or retain raw IP addresses.
- gRPC and protobuf are already direct module dependencies, so a gRPC decision
  API would not introduce a new protocol stack.

The prior distributed Olric implementation is not evidence for reviving a
distributed cache. Its non-atomic read/modify/write behavior is instead a
constraint on any future design: a shared transition must be atomic at the
bucket owner.

## Trigger criteria

Reconsider this design only when at least one measured requirement cannot be
met by local TTL caches, the existing PostgreSQL connection-cap grants, or
edge routing. Examples include:

1. A region has multiple active app processes and measurements show that
   multiplied API-key or per-player budgets cause material abuse, resource
   exhaustion, or tenant unfairness.
2. A product contract requires exact regional request or per-player limits,
   rather than the current best-effort abuse guardrail.
3. A security-sensitive override must converge across processes faster than
   the current five-second cache TTL.
4. PostgreSQL grant coordination becomes a material fraction of primary load
   and measurements show that a different coordinator would improve it. That
   is a broader cache-coordination trigger; it does not imply that the existing
   CCU grant system should automatically move.
5. An incident or controlled load test demonstrates that deterministic routing
   and Cloudflare's coarse shield cannot contain the relevant abuse case.

Before approving implementation, record the triggering measurement, required
consistency boundary (process, region, or global), target decision latency,
expected request volume, and acceptable degraded behavior. Merely adding a
second process is not by itself sufficient: the current design already accepts
bounded multiplication for abuse guardrails.

## Options to compare when a trigger fires

| Option | Advantages | Costs and limitations |
|---|---|---|
| Keep process-local buckets | No network hop or new service; current self-hosting model | A budget can multiply by the number of serving processes |
| Deterministic edge routing | Preserves local speed and gives a bucket one usual owner | Requires a stable, safe routing key; failover and membership changes reset ownership; player and action-level limits are harder to route |
| Stateful regional decision service | Exact regional ownership without another data store | New availability domain; one active instance is an enforcement single point; restart resets ephemeral buckets |
| Thin service backed by Valkey | Atomic shared state and multiple stateless service replicas | Adds a stateful dependency, another network hop, and operational cost |
| PostgreSQL per decision | Reuses an existing durable system | Adds avoidable primary load and, from the remote region, cross-country latency to every request |

Use **Valkey**, not Redis, as the named external-store candidate so the design
stays aligned with an OSI-approved dependency policy. Do not choose between a
stateful service, Valkey, and deterministic routing until the trigger supplies
real throughput, availability, and consistency requirements.

PostgreSQL should not be placed in the per-request rate-decision path. Both app
regions currently share one write primary, and remote-region access adds
cross-country latency. Keep this statement topology-neutral in implementation
documents and take the current primary location from
`docs/capacity-and-launch.md` rather than duplicating it here.

## Contingency architecture

If the evidence favors a service, use a regional rate-limit **decision
service**, not a reverse proxy that owns the complete HTTP request:

```text
client
  |
Cloudflare / regional load balancer       coarse edge protection
  |
ggscale-server
  |- tenant authentication
  |- resolve tier + cached override
  |- API-key decision -----------+
  |- player authentication       |
  |- player/IP/invite decision --+--> regional ggscale-ratelimiter
  |                                     |- atomic bucket owner, or
  |                                     `- thin API over regional Valkey
  `- application handler ------------> PostgreSQL
```

Keep the current process-local limiter as the zero-configuration self-host
default:

```text
RATE_LIMIT_BACKEND=memory  # default
RATE_LIMIT_BACKEND=remote  # managed deployment, only after a trigger
```

The application continues to own:

- API-key, player-session, and trusted-proxy authentication;
- canonical bucket identity and scope;
- tier defaults and cached PostgreSQL overrides, at least initially;
- public `429` bodies and `Retry-After` headers; and
- the failure policy for each action class.

The decision service owns:

- the authoritative time used for refill calculations;
- atomic bucket state transitions and expiration;
- request deduplication;
- atomic multi-bucket admission; and
- idempotent refunds.

Do not move the PostgreSQL-grant CCU mechanism merely because the request
limiter becomes remote. It has different state, failure, and heartbeat
requirements and must be evaluated separately.

### Regional versus global semantics

A service in each region makes limits exact across processes **within that
region**. It does not produce a globally exact east-plus-west budget. That is
appropriate while request limits remain abuse guardrails.

If a future product contract requires one global tenant budget, make that a
separate design. Viable approaches include tenant home-region affinity or
bounded token leases from a global coordinator to regional limiters. A
synchronous cross-country decision on every request is not acceptable.

## Decision API

Use a versioned internal gRPC API over persistent connections. The conceptual
contract is:

```text
CheckRequest
  operation_id
  buckets[]
    key
    refill_tokens
    refill_period
    capacity
    cost
  all_or_nothing

CheckResponse
  allowed
  retry_after
  rejected_bucket
  decision_id
  refund_expires_at

RefundRequest
  decision_id
```

Represent refill as integer tokens per duration rather than a floating-point
rate. This expresses both high-rate API limits and values such as ten per hour
or one per ten minutes without transport-level rounding.

`operation_id` makes a retried `Check` idempotent. Its retention window must be
longer than the client RPC retry window. `decision_id` makes `Refund`
idempotent; the service must retain it for a documented period derived from
the longest guarded action plus its retry window. The response exposes the
expiry. A late refund returns an explicit expired result and increments a
metric rather than silently doing nothing.

Single HTTP, IP, and player decisions normally contain one bucket. Invitation
admission uses `all_or_nothing=true` for its recipient, inviter, and domain
buckets: refill all state, reject without debiting any bucket if one is
insufficient, or debit all buckets atomically.

For the initial service version, ggscale sends the effective policy after
applying its tier and cached override. This avoids giving the limiter direct
PostgreSQL access or duplicating commercial policy. Five-second cross-process
override staleness remains accepted. If immediate lowering later becomes a
requirement, add a versioned policy/invalidation protocol rather than allowing
different app processes to repeatedly resize the same bucket with unordered
values.

## Bucket identity and privacy

Never send raw API keys, session tokens, email addresses, request bodies, or
raw IP addresses to the service. Use versioned, bounded canonical keys such as:

```text
api:v1:42
player:v1:17:981
auth-ip:v1:<HMAC>
invite-recipient:v1:project:72:<HMAC>
```

An HMAC secret used to derive shared keys must be the same on every app
process. A randomly generated per-process secret silently turns one shared
bucket back into multiple buckets. Provision a deployment-wide secret through
the existing `server_secrets`/secret-file pattern, include a key-version prefix
in bucket names, and define rotation as a coordinated rollout. Rotating it
necessarily starts new buckets unless the migration temporarily supports both
versions.

Reject unknown scopes, oversized keys, non-positive refill periods, and policy
values outside configured safety bounds. Keep all identifiers out of metric
labels and sanitize them from logs.

## Transport security

The decision endpoint is internal infrastructure, not a public API. Bind it to
the private tailnet or equivalent private network, firewall it from public
interfaces, and authenticate callers with mTLS or a dedicated service token
loaded through the `_FILE` convention. Do not forward the client's
`Authorization` header.

Version the protocol and support at least the previous compatible client
version during rollout so the app and limiter do not require a lockstep
deployment.

## Failure behavior

An unavailable limiter must not turn every request into an application-wide
`500`. Use a short regional RPC deadline and a circuit breaker, then apply an
explicit policy by action class:

| Action | Degraded behavior |
|---|---|
| General authenticated API | Use a conservative process-local bucket and record degraded admission |
| Login/signup IP protection | Use the existing local IP limiter |
| Invitations and externally costly side effects | Return `503` unless a separately justified conservative local policy exists |
| Explicit remote denial | Return the existing `429` response and `Retry-After` |

This fallback deliberately preserves availability, but it also restores the
current per-process multiplication during an outage. That limitation is part
of the go/no-go decision for building the service, not an implementation detail
to hide.

Limiter state remains ephemeral. A restart may restore burst capacity, but it
must never lose committed product data or alter billing and entitlement state.

## Shadow and rollout validation

Remote and local buckets are expected to diverge when multiple processes serve
the same key; that divergence is the purpose of the service. Shadow mode must
therefore not require request-by-request decision equality.

Use shadow mode to validate:

- stable bucket identities across app processes;
- request propagation and policy parameters;
- service latency, errors, saturation, and state cardinality;
- aggregate admission and rejection behavior over defined windows; and
- circuit-breaker and fallback behavior.

Validate token-bucket equivalence with deterministic synthetic traces in unit
and integration tests, not by assuming production shadow decisions should
match independent local buckets. Any rollout gate based on divergence must
define its expected bounds from the actual number of app processes and traffic
distribution.

If a trigger fires and an option is selected, the rollout sequence is:

1. Write tests for atomic transitions, idempotency, expiry, policy changes, and
   multi-bucket rollback before implementation.
2. Benchmark deterministic routing, a stateful service, and a Valkey-backed
   thin service against the measured requirement.
3. Add an optional remote implementation of `Limiter` and `Refunder`; retain
   memory as the default.
4. Run shadow validation for plumbing and latency.
5. Canary the shared API-key bucket in one region.
6. Exercise timeout, crash, restart, secret-rotation, and fallback drills.
7. Move IP and player buckets only after the API-key canary passes.
8. Move invitations to atomic `CheckMany` only after the local partial-debit
   defect has already been fixed and covered by regression tests.
9. Update the current architecture decision only after the canary meets the
   recorded trigger and acceptance criteria.

## Observability

At minimum, expose low-cardinality metrics for:

- decision latency and RPC errors;
- allow, deny, and degraded outcomes by scope;
- circuit-breaker state and fallback activations;
- active buckets and bucket evictions;
- idempotency hits, expired operation IDs, refunds, and expired refunds; and
- store or owner-transition failures.

Do not put tenant IDs, player IDs, IP digests, recipient digests, or complete
bucket keys in metric labels.

## Immediate independent fix: invite partial debits

Implementation plan: [`invite-throttle-partial-debit-fix.md`](invite-throttle-partial-debit-fix.md).

`internal/ratelimit/invite.go` currently checks recipient, inviter, and domain
buckets in sequence. If the recipient succeeds and the inviter rejects, the
recipient token remains consumed even though no invite is admitted. A domain
rejection can consume both earlier tokens. This is a local correctness defect,
not evidence that rate limiting must be distributed.

Fix it as a small, separate test-first change:

1. Resolve the bucket list once and track each successful debit.
2. If a later bucket rejects or returns an error, refund the earlier debits in
   reverse order through `Refunder`.
3. Keep refunds best-effort and observable; do not replace the original denial
   or error with a refund error.
4. Add behavior tests for rejection at the inviter and domain buckets, an error
   after an earlier debit, and a refund failure.

The current production `CacheLimiter` already implements `Refunder`, and the
invite create-failure path already uses that capability. This fix should land
without waiting for any shared-service trigger.

## Non-goals

- Reintroducing Olric or another embedded distributed cache.
- Making request limits into durable usage, billing, or entitlement counters.
- Moving authentication into a rate-limit gateway.
- Making all cache entries globally consistent.
- Replacing the existing regional CCU grant mechanism without separate
  measurements and design review.
- Adding a mandatory service to the zero-configuration self-host deployment.
