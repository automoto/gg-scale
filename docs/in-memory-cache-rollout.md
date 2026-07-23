# Process-local cache and regional connection-cap rollout

Status: remediation phases 1–4 and the storage media-type fix are committed in
`26909467c0ba21956b339ba3290ad48387428e71`. The two-process regional-cap,
restart, exact SIGKILL lease-recovery, exact metrics, second-tenant, browser,
and ten-minute load gates pass locally. The in-place cap-change regression test
and final result updates remain uncommitted; production rollout gates remain.

## Decision

ggscale does not run a distributed cache service. Every app process owns a
sharded in-memory cache for request rate limits, per-player socket limits, and
short-lived memoized reads. PostgreSQL remains the source of truth.

The one value that must be coordinated across the two app processes in a
region is a tenant's concurrent WebSocket envelope. Each process leases a block
of that regional capacity from PostgreSQL, then admits sockets from the block
in memory. This keeps PostgreSQL out of the per-socket heartbeat path without
introducing another stateful service.

The production shape is:

```text
Cloudflare regional pool
        |
  +-----+-----+
  |           |
app A       app B          APP_REGION=us-east
memory      memory
  | grant     | grant
  +-----+-----+
        |
PostgreSQL write primary
```

West uses the same topology with `APP_REGION=us-west`. Both regions may use the
same write primary because region is part of every capacity-state key.

## What is local and what is shared

| State | Owner | Consistency contract |
|---|---|---|
| HTTP and invite token buckets | app memory | Per-process. Cloudflare provides the coarse public-IP shield. |
| Per-player WebSocket count | app memory | Per-process safety limit, not a regional exact count. |
| Cached rate-limit overrides and other memoized reads | app memory | Writes invalidate the serving process; other processes converge by short TTL. |
| Tenant WebSocket capacity | PostgreSQL grants + app memory | Exact regional allocation ceiling while PostgreSQL is healthy. |
| Durable product state, auth, quotas, billing inputs | PostgreSQL | Authoritative. Cache loss never loses committed data. |

This tradeoff is deliberate. A process restart may briefly reduce cache hit
rate or reset a local request bucket, but it cannot corrupt durable state.

The local limits are not globally exact: with two app processes, a client that
is evenly routed can consume up to roughly twice one process's API-key bucket,
and one player can open the per-player socket limit on each process. Treat
these as abuse guardrails, not billing or entitlement counters. The regional
tenant CCU wall remains coordinated. If exact cross-process API-key or
per-player limits become a product promise, that requirement needs a shared
coordinator or deterministic routing; it must not be implied by the local
cache.

## Regional grant behavior

- Migration `0014_realtime_connection_grants` creates one cap-state row per
  `(tenant_id, region)` and one grant row per app-process boot ID.
- A boot ID is `hostname/UUID`, so overlapping Dokku generations never share a
  lease accidentally.
- A process requests blocks from 32 to 512 permits. The block is approximately
  0.5% of the tenant ceiling and is clamped at both ends.
- Admissions that fit a live local grant do not query PostgreSQL.
- All active tenant grants for one process renew in one batched PostgreSQL
  transaction every 15 seconds. The lease is 45 seconds.
- A process treats its local permits as valid for only 30 seconds after a
  successful response. The safety window prevents app/database clock skew or
  network delay from admitting against a lease PostgreSQL can already reclaim.
- As sockets disconnect, a holder returns a peak allocation to within two
  blocks of live usage. Reassessment happens under the same per-tenant lock as
  admission, so allocation can never shrink below concurrent local use.
- The final local disconnect releases the holder's grant immediately. If the
  process dies, another process reclaims the grant after expiry.
- Fully released tenant entries are removed from process memory. An hourly
  River job deletes expired grant rows left by processes that disappeared.
- Allocation is serialized by the cap-state row, so healthy processes cannot
  reserve more than the regional ceiling in aggregate.
- Established sockets are never disconnected to repair an expired or stale
  grant. New admissions stop until accounting returns under the wall.
- Burst state is persisted per tenant and region. Time above sustained drains
  the shared budget; time at or below sustained refills it.

Unused permits can temporarily strand at most two blocks of capacity on one
app. Reclaiming once slack exceeds two blocks limits database work to roughly
one reassessment per block of disconnects instead of one transaction per
socket. With two processes and the current tier ceilings, the loss is bounded
and small. Block size should be revisited from measurements, not increased
speculatively.

## Failure semantics

Grant synchronization has a 500 ms admission timeout. A transient grant-store
failure uses a process-local emergency allowance per tenant:

```text
min(64, max(8, sustained / 1000))
```

Once that allowance is exhausted, new sockets receive 503. This is bounded
degradation, not an unbounded fail-open. With two app processes, the regional
temporary overage is at most twice the per-process allowance plus permits that
were already valid.

A full PostgreSQL outage still prevents new authenticated requests because API
key resolution is database-backed. The emergency allowance primarily covers
short grant-transaction failures or contention after authentication succeeds;
it is not a database high-availability substitute. Existing WebSockets remain
up because their heartbeats use only process memory.

## Operational configuration

`APP_REGION` is the only new runtime setting. It defaults to `local` for
self-hosted development use. Production refuses the `local` default, so every
deployment must set an explicit stable region:

| Hosts | Value |
|---|---|
| `dokku-east-*` | `us-east` |
| `dokku-west-*` | `us-west` |

The bw-ops `ggscale_server` role derives `APP_REGION` from `ggscale_region`.
There are no cache backend, peer, memberlist, replica, or cache-port settings.

Relevant metrics:

- `ggscale_cache_ops_total{op,result}` — local cache activity;
- `ggscale_connection_cap_rejections_total{reason}` — ceiling, burst-budget,
  or unavailable rejections;
- `ggscale_connection_cap_grant_sync_total{result}` — grant allocation and
  renewal health;
- `ggscale_connection_cap_emergency_admissions_total` — bounded degradation;
- standard Go heap/GC and process RSS metrics — cache memory pressure.

Any emergency admission is worth investigating. Sustained grant-sync errors
are an alert; they are not normal cache misses.

## Implementation checklist

- [x] Construct only the sharded memory store at app startup.
- [x] Remove distributed-cache packages, configuration, dependencies, ports,
  tests, and cluster alerts.
- [x] Add `APP_REGION` validation and managed-host configuration.
- [x] Add PostgreSQL grant schema, RLS, least-privilege grants, and down
  migration.
- [x] Keep tenant admission on a local-grant fast path.
- [x] Batch all tenant renewals into one statement per process and interval.
- [x] Reclaim peak holder allocations as live usage falls without racing local
  admission.
- [x] Reap fully released local tenant grants and globally sweep expired
  PostgreSQL grant rows.
- [x] Remove tenant-cap refresh from socket heartbeats.
- [x] Add bounded emergency behavior instead of unbounded fail-open.
- [x] Reclaim expired nonzero local slot counters and cover high-cardinality
  cleanup under test.
- [x] Add production-role PostgreSQL tests for regional sharing, region
  isolation, batched renewal, clean reuse, and expired-process recovery.
- [ ] Complete the release gates in
  `docs/IN_MEMORY_CACHE_AND_REGRESSION_TEST_PLAN.md`.
- [x] Surface verification-email delivery failures instead of redirecting to
  the verification page after SMTP rejects a message.
- [x] Serve or explicitly declare a favicon so browser smoke runs do not issue
  a same-origin `/favicon.ico` 404.
- [ ] Confirm `APP_REGION` on both current hosts before enabling traffic.
- [ ] Deploy the new app build; its migration identity applies migration 0014
      before the server begins serving.
- [ ] Complete the one-host-at-a-time production canary and failure drill.

## Focused test-plan audit (2026-07-17)

The first regression report was directionally correct to retain a `NO-GO`, but
focused retesting corrected several pieces of evidence:

- The exact revision still does not compile: `main.go` imports the removed
  `internal/cache/build` package. The existing uncommitted startup wiring fixes
  that specific build break, but it is not part of the revision.
- Serf/memberlist are not compiled ggscale dependencies. They appear only in a
  historical transitive module graph below Agones/Viper; `go mod tidy -diff`
  is clean and the actual build/test dependency closure contains neither. The
  release gate now checks `go list -deps -test` instead of `go mod graph`.
- The reported 15-second login timeout came from the local MailHog container
  accepting TCP without sending an SMTP banner on the arm64 test host. The
  same login completed in 0.275 seconds and sent the expected message against
  a responsive SMTP peer.
- A separate SMTP rejection test confirmed a real application defect: the
  verification handler discards the delivery error and redirects as though the
  code was sent. `/favicon.ico` also reproducibly returns 404.

These corrections unblock a trustworthy rerun but do not complete the live,
browser, restart, load, or observability gates. The rollout remains `NO-GO`
until the compile and mail-error defects are fixed and the blocked rows pass on
one clean candidate revision.

## Remediation plan

Implement the fixes as four reviewable changes, then run every release gate on
one clean candidate revision. Do not combine unrelated refactoring with these
changes.

### Phase 1 — restore a clean, compiling candidate

Working-tree status: implemented and compiling. The server package and full
unit/integration suites pass, but the changes still need to be committed before
the release candidate is immutable.

1. Preserve the existing process-local cache startup change, but land it as a
   complete server-wiring change:
   - construct `instrument.New(memory.New(), registry)`;
   - construct and close `ratelimit.NewPostgresConnectionCap`;
   - inject the tenant cap into the HTTP/realtime dependencies;
   - register the connection-grant GC worker and periodic job.
2. Confirm no deleted cache configuration or `internal/cache/build` reference
   remains in the compiled source.
3. Run `go test ./cmd/ggscale-server` first, followed by `make lint` and
   `make test`. The phase is complete only when those commands pass from a
   clean worktree containing the intended candidate.

### Phase 2 — make verification-email failure explicit and retryable

Working-tree status: complete. Rejecting-mailer integration tests cover control
panel setup/login/resend and player fresh/duplicate signup plus resend. Failed
delivery returns a safe 503, restores the prior code/cooldown state, permits an
immediate retry, increments the existing error metric, and cannot overwrite a
newer concurrently installed code.

Write the failure-path tests before changing the handlers.

1. Add rejecting-mailer tests for all user-facing verification entry points:
   - control-panel setup, unverified login, and resend;
   - player-account signup and resend;
   - duplicate player-account signup, preserving the existing
     anti-enumeration response contract.
2. Assert that an SMTP rejection never produces a success redirect or a
   “code sent” message. Return a user-safe `503`/retry response without
   exposing the recipient, SMTP response, or verification code.
3. Change `startVerification` and `sendAccountVerifyEmail` to return wrapped
   mailer errors. Handle those errors at every caller instead of discarding
   them. Make the account-exists notification return an error too, but keep
   its externally visible result indistinguishable from fresh signup.
4. Keep the current persist-before-send ordering so an email can never contain
   a code the database failed to store. Add a conditional compensating query,
   guarded by the newly written code hash, that restores the prior verification
   state or clears the delivery reservation after a failed send. This must:
   - leave a previously delivered code valid when a later resend fails;
   - permit an immediate retry instead of enforcing a cooldown for mail that
     was never delivered;
   - avoid clobbering a newer code created by a concurrent request.
5. Add a concurrency test for the guarded compensation and an integration test
   with an SMTP peer that rejects `MAIL FROM`. Assert the HTTP result, database
   state, retry behavior, and the existing
   `ggscale_mail_sends_total{result="error"}` metric delta. Never log message
   bodies or verification codes.

Phase 2 is complete when successful delivery still follows the existing
verification flow, failed delivery is visible and retryable, and fresh versus
duplicate player signup remains indistinguishable.

### Phase 3 — eliminate the favicon request failure

Working-tree status: complete. Both rendered heads reference a content-hashed
embedded SVG, and `/favicon.ico` serves it with `image/svg+xml`, immutable
cache policy, and `nosniff`.

1. Add tests first for a versioned embedded favicon, both rendered page heads,
   and the legacy `/favicon.ico` route.
2. Add a small repository-native SVG favicon to `internal/webassets/static`,
   reference it from both `baseHead` and `playerHead`, and regenerate the templ
   output.
3. Make `/favicon.ico` serve or redirect to the embedded asset so direct and
   legacy requests do not return 404. Verify the content type, cache policy,
   CSP compatibility, and `X-Content-Type-Options` behavior.

Phase 3 is complete when control-panel and player pages load with no favicon
404 or console error and all asset/template tests pass.

### Phase 4 — repair the local mail test harness

Working-tree status: complete. Compose now pins multi-architecture Mailpit
v1.30.0, waits for its health check, and supports overrideable loopback-only
host ports. The pre-production plan includes a disposable curl-based check that
requires a working SMTP protocol, sends one uniquely addressed message, and
reads it back through `/api/v1/search` before browser fixtures are created.

1. Replace or repin the SMTP capture service with a maintained image that
   supports both amd64 and arm64, keeping Linux as the only CI platform. If its
   HTTP API differs from MailHog, update the test helpers and documentation in
   the same change.
2. Add a protocol-level readiness probe that requires a prompt SMTP `220`
   banner and an HTTP API response. A listening TCP socket alone is not ready.
3. Keep topology-specific addresses explicit: containers use the Compose
   service name, while host-launched app processes use `127.0.0.1:1025`.
4. Add a disposable-stack smoke test that sends one uniquely addressed message
   and reads it back through the capture service API before browser fixtures
   are created.

### Phase 5 — release verification and closeout

Status: automated and disposable-stack portions pass; the full two-process
LIVE/REG browser, restart/load/observability, and production canary portions
remain pending and therefore the rollout verdict remains `NO-GO`.

Run these checks against the same clean candidate, in order:

1. `make lint`, `make test`, and
   `INTEGRATION_PARALLEL=2 make test-integration`;
2. repeated cache/grant race suites from the regression plan;
3. the two-process east/east ceiling, east/west isolation, release, expiry,
   renewal, emergency, and metric-delta cases;
4. the full REG matrix with fresh browser contexts and the repaired mail
   harness;
5. restart/load/observability checks, including heap reclamation and grant
   query shape.

Update both rollout documents with command exit codes and evidence paths.
Change the verdict to `GO` only when the exact tested revision compiles, every
critical row passes, no browser request fails, and no required row remains
blocked.

### Remediation verification — 2026-07-17

The working-tree candidate passed:

- `make lint` (0 issues), `make test`, and
  `INTEGRATION_PARALLEL=2 make test-integration`;
- the SMTP-rejection/guarded-compensation integration suite under `-race`;
- cache and rate-limit suites under `-race -count=20`;
- PostgreSQL cap/grant and expired-grant GC suites under `-race -count=10`;
- Mailpit v1.30.0 health, SMTP 220, unique-message API readback, and `make e2e`;
- a live `/favicon.ico` request returning 200 with SVG, immutable cache, and
  `X-Content-Type-Options: nosniff`.

Evidence is retained at `/private/tmp/ggscale-remediation-20260717`. The exact
HEAD in that directory is the pre-remediation revision, so this evidence must
not be used to declare `GO`; rerun the remaining gates after committing one
candidate revision.

Focused manual closeout also passed on the final cleaned working tree: Mailpit
accepted and returned a uniquely addressed SMTP message, a real player signup
redirected to verification and emitted mail, a duplicate returned the same
status and Location, and the live favicon route plus both rendered heads were
correct. The disposable account/messages were removed and the stack was
stopped afterward. The permanent coverage is limited to the product regression
tests; the temporary mail-smoke command/package and redundant E2E test were
removed.

### Production-readiness continuation — 2026-07-18

The committed remediation revision `bb88aa744a841f7e95b25ad3c2aaec4f5cef6929`
passed `make lint`, `go test -race ./...`, the cache/ratelimit race repeat, and
`INTEGRATION_PARALLEL=2 make test-integration`. The final HTTP integration
package completed in 426.891 seconds.

Manual live verification used two `us-east` processes and one `us-west`
process against the retained PostgreSQL 17 database. The hard-cap phase used
`REALTIME_MAX_PER_TENANT=4`; the renewal/load phase used 64 so both east
holders could retain a 32-permit grant.

- The east storm admitted exactly 4 of 100 authenticated WebSocket dials and
  rejected 96 with 503. Closing an admitted socket made one replacement
  available. West independently admitted 4 and rejected the next 4.
- A lost east holder continued to block its allocation immediately, then the
  peer admitted four after the 45-second lease window (47-second test poll).
- During the ten-minute run, PostgreSQL showed two `us-east` holders with
  `allocated=32`, `used=1`, and renewed 44–45-second leases. The uninterrupted
  holder recorded 41 successful syncs, matching one initial allocation plus
  approximately one batched renewal every 15 seconds. Emergency admissions
  remained zero.
- Mixed traffic completed 1,369 successful HTTP operations, 71 expected 429
  responses, and 288 successful WebSocket connect/close cycles. The restarted
  process recorded 36 expected transport failures during its outage; the peer
  returned 200 throughout. Neither process recorded an application 5xx.
- Baseline RSS was 39.6–40.0 MB with a 4.0 MB Go heap. Immediate post-load RSS
  was 43.8–44.0 MB; after traffic stopped and cleanup ran, RSS was 42.7–43.1 MB
  and heap returned to 4.4–4.5 MB. Cache counters stopped moving and no
  monotonic growth was observed.
- The test-created API key, Casbin role, 17 anonymous players, audit/session/
  storage rows, secret-bearing response files, probe source/binaries, and host
  test processes were removed. The pre-existing Compose stack was left
  running.

The continuation also reconfirmed PRE-PROD `EDGE-05`: storage accepted valid
JSON sent as `text/plain`. The working tree now binds the request body as
`json.RawMessage`, so Huma enforces `application/json` while accepting any
valid JSON value. The regression test proves 415 with no stored row; a rebuilt
live server returned 415 for `text/plain`, 200 for `application/json`, and 204
when the disposable object was deleted. `openapi.yaml` now advertises an
open-ended `application/json` body. Final lint, race-enabled unit tests,
focused positive/negative storage integration tests, and the complete
integration suite pass.

The previously blocked browser checks were rerun with the direct Playwright
MCP against the rebuilt Compose server. Control-panel login, email verification
through Mailpit, authenticated rendering, and logout passed with all required
assets returning 200 and no console errors. Player login, account rendering,
and logout also passed with no console errors on the successful flow. For
`BF-TIER-01`, the platform-admin selector changed tenant 9 from `tier_0` to
`tier_1`, rendered the new tier and 1000/2000 defaults after reload, and then
changed it back to `tier_0`; the restored page rendered the original 150/300
defaults. Screenshots are retained in
`/private/tmp/ggscale-prodready-browser-20260718`.

The formerly blocked live second-tenant, in-place cap-change, exact SIGKILL,
and exact metric-delta cases now pass. On the final 2026-07-18 working tree,
lint, the complete race-enabled unit and integration suites, the endpoint e2e
suite, OpenAPI regeneration/diff, migration 14 state, and rebuilt Compose
health also pass. Fresh browser smoke covers control-panel and player login,
rendering, logout, and the platform tier selector; it does not imply that every
REG workflow was repeated during this continuation.

Verdict remains **NO-GO** until the grant regression test and result updates
are committed and the evidence is bound to that immutable revision, the real
production host and expected CORS allowlist are available for read-only smoke,
production `APP_REGION` is verified on both regions before deployment, and the
one-host-at-a-time production canary/failure drill completes successfully.

## Deployment and rollback

1. Configure `APP_REGION` on east and west through bw-ops before deploying the
   application.
2. Deploy one current app host. Its migration identity applies migration 0014
   before startup. Confirm the migration completed, health is green,
   grant-sync succeeds, emergency admissions stay at zero, and no new 5xx rate
   appears.
3. Exercise WebSocket connect/close. Verify peak allocation decays as usage
   falls and the grant row disappears after the last disconnect.
4. Deploy the other region and repeat.
5. Before adding second hosts, run the two-process regional tests and crash
   recovery drill from the test plan.

Rollback the app build first. The two new tables are inert for the old build
and can remain in place. Do not run the down migration during an incident; drop
the tables only in a later maintenance window after every process using grants
is gone.

## Later decision triggers

Do not add a separate cache service preemptively. Reconsider one only if
measurements show a requirement that PostgreSQL grants and local TTL caches
cannot meet, such as exact regional per-player limits, cross-process cache
invalidation below the current TTL, or database grant traffic becoming a
material fraction of primary load. At that point, use
[`cache-improvement.md`](../cache-improvement.md) as the contingency design and
compare deterministic routing, a Valkey-backed thin service, and a dedicated
coordination service against the measured need.
