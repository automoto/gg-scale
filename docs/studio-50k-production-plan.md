# Studio 50,000-connection production plan

Status: enforcement code and public documentation are implemented. Production
deployment and representative capacity verification remain to be recorded.

## Objective

Raise Studio to 50,000 sustained realtime connections per tenant, per ggscale
service region, with a temporary maximum of 100,000. Preserve the existing
10-minute full-2× burst model initially.

Maintain the API sizing rule of one backend action per connected player every
10 seconds by raising Studio's per-key API refill rate from 2,500/sec to
5,000/sec. Size the bucket for one immediate action per sustained connection:
50,000 requests per Studio API key.

Move the Enterprise starting connection envelope from 50,000/100,000 to
100,000/200,000 so it remains above Studio. Its existing 10,000/sec API rate
already matches the reference rule for 100,000 connections.

## Non-goals

- This change does not increase dedicated-game-server, relay, or fleet capacity.
- It does not turn an admission limit into an uptime or latency guarantee.
- It does not add automatic paid overages or a multi-day launch burst.
- It does not claim that an admission limit is verified endpoint throughput.

## Release blockers

### 1. Distributed realtime delivery

The current realtime `Hub` is process-local, while each application process can
run a matchmaker worker. A worker that owns a match may not own the target
player's WebSocket. Before horizontal scale is approved, add a distributed
connection-owner registry and cross-process delivery path, or establish and
test an equivalent topology invariant.

Required behavior:

- any worker can deliver to a player connected to any healthy application node;
- reconnecting a player replaces the prior route without leaving a stale owner;
- node loss expires or transfers ownership without waiting indefinitely;
- duplicate delivery is detectable and harmless;
- rolling deploys drain or migrate delivery without losing match results.

### 2. Tenant-specific connection overrides

The service now has a persisted, auditable connection-limit override for
controlled launches. It supports sustained and temporary limits, validates
that the temporary limit is between sustained and 2× sustained, enforces an
absolute 500,000 safety wall, converges across nodes within five seconds, and
appears in the control panel and audit log. An active
`REALTIME_MAX_PER_TENANT` is visible and disables conflicting writes.
Production operators still need to exercise the workflow before a pilot.

Concurrent override-cache misses are singleflight-coalesced per tenant. A
failed refresh serves the last known override or tier default with a one-second
retry backoff, records a metric, and continues through the leased cap so its
bounded database-outage allowance remains effective.

### 3. Region-wide API rate enforcement

HTTP API token buckets are currently process-local. Managed production runs one
web process in each region, so the current effective scope is per API key, per
service region. Before adding web processes for a hit launch, implement a
shared regional limiter or a tested quota-division scheme. Verify aggregate
rate and burst across at least two processes; multiplying the advertised bucket
by the process count is not acceptable.

### 4. Capacity observability

Add low-cardinality regional metrics and customer-facing usage data for:

- current and peak connections by service class and region;
- percentage of sustained and temporary capacity in use;
- burst budget remaining;
- admission attempts and rejections by reason;
- connection churn and handshake latency;
- cross-process delivery attempts, failures, retries, and duplicates;
- API utilization and throttling by route class;
- database pool, cache, CPU, memory, file-descriptor, and network saturation.

Alert at 70%, 85%, 95%, and 100% of sustained capacity. Page operators on
unexpected rejection growth, emergency admissions, grant synchronization
failure, or realtime delivery failure.

### 5. Repeatable workload harness

Create a versioned load harness that opens authenticated WebSockets, exercises
representative HTTP routes, receives directed realtime messages, and records
end-to-end correctness and latency. It must support multiple load generators so
the client side is not the bottleneck.

The workload mix must be configurable and checked into the repository with the
result schema and runbook. Do not treat idle-socket counts as sufficient proof.

### 6. Unit economics

Measure infrastructure cost per 1,000 sustained connections, per million
realtime messages, and per million API operations in every production region.
Include idle headroom, database replicas, observability, cross-region services,
and a single-node failure reserve. Product signs off on the target gross margin
before the public limit changes.

## Verification matrix

Run each profile on the intended production topology. Record application
version, region, node shape/count, database shape, cache configuration, and load
harness version.

| Profile | Target | Minimum duration | Purpose |
| --- | ---: | ---: | --- |
| Idle connection soak | 50,000 | 2 hours | File descriptors, memory, heartbeat, grants, and connection stability |
| Reference mixed load | 50,000 plus 5,000 API req/s | 1 hour | Published sustained envelope and API sizing rule |
| Directed event delivery | 50,000 connected recipients | 30 minutes | Cross-node routing correctness and delivery latency |
| Reconnect wave | Replace 25,000 connections over 5 minutes | Until stable | Authentication, churn, stale-route cleanup, and grant reuse |
| Full temporary maximum | 100,000 | 10 minutes plus recovery | Ceiling, burst accounting, and recovery below sustained |
| Regional isolation | 50,000 in each of two regions | 30 minutes | Independent regional envelopes and shared tenant behavior |
| Node failure | 50,000, then terminate one node | Until recovered | Capacity reserve, route expiry, reconnect behavior, and delivery |
| Rolling deployment | 50,000 | Full deployment | Drain behavior and uninterrupted admission/delivery |
| Grant-store impairment | 50,000 | Failure and recovery cycle | Emergency allowance, bounded failure, and lease recovery |

The mixed profile must include authentication, token refresh, representative
reads and writes, matchmaking queue operations, and directed realtime events.
Apply realistic jitter rather than issuing every player action at the same
instant, and add separate worst-case spike runs.

## Acceptance gates

All gates must pass before changing the compiled Studio default:

- no connection-cap rejection below 50,000 when burst budget is not involved;
- no admission above 100,000 in one regional envelope;
- one API key remains bounded to 5,000/sec sustained and a 50,000-request
  bucket across two or more web processes in one service region;
- correct 10-minute budget consumption at 100,000 and one-hour refill behavior;
- zero lost cross-node match notifications in deterministic delivery tests;
- no stale connection ownership after reconnect, node loss, or lease expiry;
- handshake and API latency remain within the production SLOs;
- unexpected HTTP 5xx responses remain below the production error-budget
  threshold, excluding deliberate capacity rejections;
- the system retains at least 25% CPU, memory, database, and network headroom at
  50,000 after accounting for the approved failure scenario;
- no sustained file-descriptor, goroutine, connection-pool, or queue growth
  after the workload returns to baseline;
- the regional cost model passes product and finance review;
- operations approves dashboards, alerts, runbooks, and rollback access.

## Rollout sequence

### Phase 0: Documentation and baseline

- [x] Document current connection, burst, regional, and API-key semantics.
- [x] Correct the public pricing explanation for the 50,000-connection Studio
      envelope and per-key API bucket.
- [ ] Capture the pre-change 25,000-connection baseline results and
      infrastructure cost, if available.

### Phase 1: Scale prerequisites

- [ ] Implement and test distributed realtime delivery.
- [ ] Implement and test region-wide API-key rate enforcement before
      horizontally scaling the web process.
- [x] Add tenant-specific connection overrides with audit history.
- [x] Bound overrides to 2× sustained and an absolute 500,000 safety wall.
- [x] Preserve realtime admission fallback and coalesce override lookups during
      database failures and reconnect waves.
- [x] Decouple publishable-key auth abuse limits from the larger API buckets.
- [x] Expose deployment-wide environment-cap precedence in the control panel.
- [ ] Add capacity gauges, burst visibility, dashboards, and alerts.
- [ ] Build and document the repeatable workload harness.
- [ ] Verify OS, load balancer, proxy, and file-descriptor limits above 100,000
      connections per regional failure domain.

### Phase 2: Internal validation

- [ ] Run the complete verification matrix in a production-like environment.
- [ ] Fix every correctness failure before interpreting performance results.
- [ ] Tune node count, database pools, caches, timeouts, and connection routing.
- [ ] Repeat until every acceptance gate passes twice on separate runs.

### Phase 3: Controlled production pilot

- [ ] Select an internal or design-partner Studio tenant with an explicit test
      window and rollback contact.
- [ ] Apply a 50,000/100,000 tenant override in one production region.
- [ ] Start at 10% of target traffic and progress through 25%, 50%, 75%, and
      100%, holding each stage long enough to evaluate alerts and trends.
- [ ] Exercise one controlled node failure and one rolling deployment.
- [ ] Hold the pilot at target for at least one representative peak period.
- [ ] Complete an operational review and record the production cost.

### Phase 4: Default change

- [x] Write failing unit tests for the new tier values before implementation.
- [x] Change Studio connections to 50,000 sustained and 100,000 temporary.
- [x] Change Studio API defaults to 5,000/sec and a 50,000-request bucket.
- [x] Change the Enterprise starting connection envelope to
      100,000/200,000.
- [x] Update tier, token-IP, connection-cap, override, integration, and control
      panel tests.
- [ ] Deploy the backend migration and enforcement changes.
- [ ] Confirm effective production limits, dashboards, alerts, and rollback.

### Phase 5: Public launch

- [x] Prepare the Studio pricing card and technical table for 50,000 sustained,
      100,000 temporary, 5,000/sec API rate, and a 50,000-request bucket.
- [x] Remove the release-gate comment and deploy the prepared pricing change.
- [x] Publish the per-service-region scope and launch-capacity contact path.
- [ ] Update sales enablement, support macros, order forms, and status runbooks.
- [ ] Monitor the first 30 days and review utilization, rejections, reliability,
      and gross margin weekly.

## Rollback

The rollback must preserve established WebSockets. Lower the effective limit
back to 25,000/50,000 for new admissions through the tenant override or a
backend rollback; the grant allocator already preserves live usage and stops
issuing new permits until the regional total returns below the active wall.

If the API increase causes instability, restore Studio's 2,500/sec rate and
5,000-request bucket independently. If distributed delivery is unhealthy,
stop the pilot, drain the affected topology, and restore the last known-good
routing path.

Do not leave public documentation at 50,000 if the production default is rolled
back. Customer communication must state whether already-connected players are
affected and when new admissions will recover.

## Final approval record

Before Phase 5, attach or link:

- two passing verification reports;
- the controlled-pilot report;
- the unit-economics review;
- the production dashboard and alert definitions;
- the incident and rollback runbook;
- engineering, operations, product, and finance approvals.
