# In-memory cache and major-feature regression test plan

Audience: an autonomous test agent with shell, Docker, HTTP, PostgreSQL, and
Playwright browser access.

Goal: heavily test the process-local cache and PostgreSQL regional connection
grants, then perform a broad regression pass across ggscale's major API and web
features. `docs/PRE_PROD_TEST_PLAN.md` remains the exhaustive security and
feature catalog; this plan is the executable release gate for the cache
redesign.

## Release decision

The revision passes only when all of these are true:

1. `make lint`, `make test`, and `make test-integration` pass on the same
   revision with the race detector enabled by the Make targets.
2. Cache stress tests pass repeatedly without a race, panic, deadlock,
   overspend, over-admission, or retained expired keys.
3. Two app processes in one region enforce one tenant ceiling; another region
   has an independent ceiling.
4. Clean release, process-loss expiry, renewal, and transient grant failure
   match the documented bounds.
5. All critical security cases and every major-feature smoke row below pass.
6. Browser evidence contains no uncaught console exception, failed same-origin
   request, broken navigation, or secret rendered in the page.

Any cross-tenant data leak, privilege escalation, regional CCU overshoot while
PostgreSQL is healthy, unbounded failure admission, cache race, quota bypass,
or migration/RLS failure blocks release.

## Agent rules and evidence

- Test only a disposable local environment unless the operator explicitly
  authorizes a production read-only smoke pass.
- Never weaken production constants or RLS policies to make a test pass. Use
  test-only short leases/budgets in Go integration tests.
- Create users, memberships, and API keys through product flows. Raw inserts
  can omit Casbin relationships and create false failures.
- Use a fresh Playwright browser context for each role. Never reuse an owner or
  platform-admin session for a lower-privilege assertion.
- Prefer accessible roles, labels, and visible text over brittle CSS selectors.
- After each browser action, assert the URL, visible success/error state, and
  the backing API response or durable state where practical.
- Capture secrets only in a restricted run manifest. Redact them from logs,
  screenshots, filenames, and the final report.
- Do not mark a case passed from an HTTP status alone when the case changes
  state; read the state back.

Create an evidence directory outside the repository:

```bash
export GGSCALE_TEST_RUN="/private/tmp/ggscale-memory-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$GGSCALE_TEST_RUN"/{logs,screenshots,http,metrics,db}
git rev-parse HEAD | tee "$GGSCALE_TEST_RUN/revision.txt"
go version | tee "$GGSCALE_TEST_RUN/go-version.txt"
docker version | tee "$GGSCALE_TEST_RUN/docker-version.txt"
```

Maintain `results.md` in that directory with columns: ID, result, duration,
revision, topology/config, evidence paths, and notes. Use `PASS`, `FAIL`, or
`BLOCKED`; never silently skip a row. Record the first failing assertion and
continue only when it cannot contaminate later results.

## Phase 0 — static and automated gates

Run from the repository root:

```bash
make lint 2>&1 | tee "$GGSCALE_TEST_RUN/logs/lint.log"
make test 2>&1 | tee "$GGSCALE_TEST_RUN/logs/unit-race.log"
INTEGRATION_PARALLEL=2 make test-integration 2>&1 \
  | tee "$GGSCALE_TEST_RUN/logs/integration-race.log"
```

Then repeat the cache and capacity packages to expose timing-sensitive bugs:

```bash
go test -race -count=20 ./internal/cache/... ./internal/ratelimit \
  2>&1 | tee "$GGSCALE_TEST_RUN/logs/cache-repeat-race.log"
go test -race -tags=integration -count=10 -run 'TestPostgres(ConnectionCap|GrantStore)' \
  ./internal/ratelimit 2>&1 \
  | tee "$GGSCALE_TEST_RUN/logs/grant-repeat-race.log"
go test -race -tags=integration -count=10 -run 'TestSweepExpiredConnectionGrants' \
  ./tests/integration/jobs 2>&1 \
  | tee "$GGSCALE_TEST_RUN/logs/grant-gc-repeat-race.log"
```

Inspect the dependency and configuration surface:

```bash
go list -deps -test ./... | rg -i 'distributed.cache|memberlist|serf' \
  | tee "$GGSCALE_TEST_RUN/logs/removed-cache-deps.log"
rg -n -i 'cache_backend|cache_.*peer|cache_.*member|cache_.*replica' \
  .env.example docker-compose.yml internal cmd db infra \
  | tee "$GGSCALE_TEST_RUN/logs/removed-cache-config.log"
```

Both searches must be empty. A search command's exit code 1 means no matches
and is expected. Do not use `go mod graph` as the release assertion here: it
prints the complete declared graph of selected modules, including packages
that ggscale never compiles. For example, Agones selects Viper, whose historical
module graph reaches Consul, Serf, and memberlist even though none of those
packages are in ggscale's build dependency closure. `go mod graph` remains
useful for explaining an unexpected compiled dependency, not for deciding this
gate by itself.

### Cache torture matrix

| ID | Required behavior | Automated evidence |
|---|---|---|
| MEM-01 | Same-key token bucket admits exactly capacity under 2,000 simultaneous callers | `TestStoreConcurrentTokenBucket_never_spends_past_capacity` |
| MEM-02 | Plain slot counter never exceeds its limit under 2,000 callers | `TestStoreConcurrentSlots_never_admit_past_limit` |
| MEM-03 | Burst counter is atomic under contention and distinguishes sustained/ceiling | cache store contract suite |
| MEM-04 | Token refill, retry duration, refunds, and key isolation are correct | cache store contract and burst unit tests |
| MEM-05 | Set/Get copy bytes, TTL expires, delete is idempotent, misses are typed | cache store contract and memory tests |
| MEM-06 | Delete clears plain, burst, bucket, and KV namespaces for the key | `TestStoreDelete_removes_burst_slot` plus store contract |
| MEM-07 | Expired counters are reclaimed even if a vanished holder left count nonzero | `TestStoreSweep_removes_expired_live_slot_counters` |
| MEM-08 | At least 20,000 expired high-cardinality keys are reclaimed in one sweep | `TestStoreSweep_reclaims_high_cardinality_expired_entries` |
| MEM-09 | Shards remain race-free across repeated mixed operations | `-race -count=20` package run |
| MEM-10 | Closing the store stops cleanup and is idempotent | memory/store contract tests |
| MEM-11 | Restart loses only cache state; durable database state remains unchanged | Phase 2 restart case |
| MEM-12 | Metrics distinguish hit/miss/deny/error without tenant or player labels | instrument tests plus Phase 2 metrics capture |

For MEM-08, record duration and peak RSS from the test process if available.
Fail if cleanup leaves entries, runtime grows nonlinearly across repeat runs, or
the race detector reports any access.

## Phase 1 — regional grant integration

The automated PostgreSQL tests must connect as the production app role, not as
the table owner. They cover RLS as well as behavior.

| ID | Scenario | Expected |
|---|---|---|
| GRANT-01 | Two holders, same tenant and region, hard ceiling four | Aggregate allocation/admission never exceeds four. |
| GRANT-02 | Same tenant, east and west | Each region has an independent envelope. |
| GRANT-03 | Last local connection closes | Holder grant is released and another holder can reuse capacity immediately. |
| GRANT-04 | Holder disappears without release | Its grant blocks reuse only until the 45-second production lease; short test lease proves recovery. |
| GRANT-05 | Multiple active tenants renew | One batched store call/transaction renews all grants for the process. |
| GRANT-06 | Tenant-scoped allocate/release under RLS | App role sees only the request tenant; worker renewal sees only rows selected by holder/region. |
| GRANT-07 | Valid local block has free permits | Repeated admissions do not call PostgreSQL. |
| GRANT-08 | Local block fills | One synchronization obtains the next bounded block; no permit is issued past allocated capacity. |
| GRANT-09 | Grant transaction fails | Emergency admissions stop at `min(64,max(8,sustained/1000))`; the next request is rejected as unavailable. |
| GRANT-10 | Grant expires while sockets remain | Existing sockets are not dropped; new capacity waits for successful reconciliation or uses only the bounded emergency allowance. |
| GRANT-11 | Cap configuration changes | Next synchronization applies the new sustained/ceiling values without dropping established sockets. |
| GRANT-12 | Metrics | Sync success/error, rejection reason, and emergency counters move by the expected exact deltas. |
| GRANT-13 | One holder peaks, then most sockets disconnect | Its allocation falls to at most two blocks above live usage, allowing another holder to reuse the returned capacity. |
| GRANT-14 | Many tenants fully disconnect | Their local grant-map entries are reaped; an acquire waiting behind the final release retains the same grant safely. |
| GRANT-15 | A process dies and its tenant stays quiet | The app-role River sweep deletes the expired orphan across tenants without deleting a live row. |

Run migration down/up on a disposable database and confirm migration 0014 is
reversible. Confirm the app role has only the required table privileges and
that a request transaction cannot read another tenant's cap rows.

## Phase 2 — live two-process behavior

Start from a clean local stack and seed it:

```bash
make clean
make up
make seed
docker compose stop ggscale-server
go build -o /private/tmp/ggscale-server ./cmd/ggscale-server
```

Launch two binaries against the same local PostgreSQL database on different
HTTP ports. Set `APP_REGION=us-east` and
`REALTIME_MAX_PER_TENANT=4` on both. Use the normal development mail and
cookie settings from `docker-compose.yml`, but translate container-only
addresses to their published host addresses: a host-launched binary uses
`SMTP_ADDR=127.0.0.1:1025`, not `mailpit:1025`. Give each process a separate
log file. If the launch environment requires additional secrets, use the same
development values for both processes; do not copy production credentials.

Before provisioning users, verify the SMTP service speaks SMTP rather than
merely accepting TCP. A successful probe must receive a `220` banner promptly.
If the port connects but no banner arrives, mark mail-dependent cases
`BLOCKED`, repair or replace the local mail service, and do not assign the
resulting request timeout or empty inbox to ggscale.

Use the disposable send/readback commands in
`docs/PRE_PROD_TEST_PLAN.md` before creating browser fixtures. They exercise
the SMTP protocol and Mailpit API without adding a permanent test-only binary
to the application.

Create valid API keys and at least six players through the control panel/API.
Route WebSocket dials directly to both ports so the distribution is known.

| ID | Action | Expected |
|---|---|---|
| LIVE-01 | Release 100 authenticated WebSocket dials from one barrier, alternating ports | Exactly four accepted across both processes; all others get 503 and `Retry-After: 5`. |
| LIVE-02 | Close one accepted socket, then dial both ports | Exactly one replacement succeeds. |
| LIVE-03 | Dial another tenant | Its four permits are independent. |
| LIVE-04 | Restart one app, leaving the other up | Durable features stay correct; local memoized data repopulates; no stale cache decode/error loop. |
| LIVE-05 | Kill a holder with sockets using SIGKILL | Its allocation becomes reusable no later than 45 seconds plus test polling tolerance. |
| LIVE-06 | Run a third process with `APP_REGION=us-west` | Same tenant can admit four west sockets independently of four east sockets. |
| LIVE-07 | Hold sockets for 60 seconds | Heartbeats succeed; database logs show batched grant renewal, not one tenant-cap query per heartbeat/socket. |
| LIVE-08 | Force only the grant store to fail in a test seam | Admissions are bounded by GRANT-09; do not claim a full-DB-outage availability test because API-key auth itself requires PostgreSQL. |
| LIVE-09 | Scrape `/metrics` before/after | Exact expected counter deltas; no high-cardinality identity labels. |
| LIVE-10 | Send one API key and one player alternately to both processes | Request buckets and per-player caps are independent per process; record the aggregate multiplier explicitly. Tenant CCU remains regional. |

For the synchronized storm, retain a machine-readable record per attempt:
target process, HTTP result, WebSocket upgrade result, tenant/player, start/end
timestamp, and close result. Count success from completed upgrades, not only
HTTP responses.

## Phase 3 — Playwright major-feature regression

Use `docs/PRE_PROD_TEST_PLAN.md` for exact fixture and authorization details.
This phase is a general regression, so every row below must receive at least
one happy-path assertion and one meaningful deny/validation assertion.

Before testing, attach Playwright listeners for console errors, page errors,
failed requests, and responses with status 500+. Save them per case. Take a
screenshot after the final asserted state, not during a loading transition.

| ID | Surface | Browser/API checks |
|---|---|---|
| REG-01 | Health and API contract | `/v1/healthz`, OpenAPI, API version header, unknown route/method behavior. |
| REG-02 | Control-panel authentication | Login/logout, wrong password, CSRF denial, session expiry/revocation, password change. |
| REG-03 | Tenant onboarding | Request access, email in Mailpit, approval/denial, accept link, duplicate/expired link. |
| REG-04 | Tenant dashboard | Projects, tenant/project settings, tier/usage display, validation and persisted edits. |
| REG-05 | Team and RBAC | Owner invite, accept, role change/revoke; admin/member forged privileged actions denied. |
| REG-06 | API keys | Create publishable/secret keys with permitted roles, one-time secret display, revoke, revoked key denied. |
| REG-07 | Player web authentication | Signup, email verification, login/logout, reset/change password, 2FA if configured. |
| REG-08 | Player profile/linking | Profile view/edit, project linking/invite accept, duplicate/foreign invite denial. |
| REG-09 | Friends and presence | Request/accept/list/block/unblock/delete, offline/online state, cross-tenant opaque denial. |
| REG-10 | Sessions and game invites | Create/join/leave/verify session, invite friend, expired/unauthorized invite denial. |
| REG-11 | Leaderboards | Create/edit/delete definition, submit score with secret key, top/around-me reads, publishable write denied. |
| REG-12 | Player storage | Put/get/list/update/delete, ETag conflict, max body/quota denial, tenant/project isolation. |
| REG-13 | Matchmaking | Create/status/cancel ticket, per-player limit, scope denial, successful match notification path. |
| REG-14 | Realtime WebSocket | Upgrade, hub delivery, reconnect, per-player cap, regional tenant cap, 503 retry contract. |
| REG-15 | Platform administration | Users/player accounts, tenant settings/tier, signup queue, plugins/settings; tenant owner denied. |
| REG-16 | Quotas and change requests | Project/player/storage enforcement, no-growth operations, upgrade request approve/deny, stale downgrade protection. |
| REG-17 | Fleet/server browser | When enabled: fleet CRUD, allocation/reprovision, secret-key/scope enforcement. Otherwise verify hidden/denied. |
| REG-18 | Relay | When enabled: credential issuance and scope/key-type enforcement. Otherwise verify hidden/denied. |
| REG-19 | Email workflows | Correct recipient, purpose, single delivery, sanitized content, expired/replayed links denied. |
| REG-20 | Tenant isolation sweep | Repeat critical cross-tenant API and UI reads/writes from PRE_PROD §12; no data or effect crosses tenants. |

Browser roles must include platform admin, Tenant A owner, Tenant A admin,
Tenant A member, Tenant B owner, and two players in different tenants. Save the
storage state for each context so a failure can be replayed, but never commit
those files.

For every destructive case, use a named throwaway fixture and run it after the
read/happy-path checks that depend on that fixture. Verify UI actions against a
fresh page load and, where possible, a direct API or database read.

## Phase 4 — restart, load, and observability

1. Generate at least 10 minutes of mixed traffic: API rate-limited reads,
   leaderboard reads, storage reads/writes, control-panel navigation, and live
   WebSockets split across both app processes.
2. Restart one process during traffic. Confirm only requests routed to that
   process see normal restart disruption; the peer continues serving.
3. Confirm local cache misses rise on the restarted process and then settle.
4. Check process RSS and Go heap before load, at peak key cardinality, after
   TTL plus a sweep interval, and after traffic stops. Expired-cache memory
   must be reclaimed; investigate monotonic growth over repeated cycles.
5. Check PostgreSQL query rate. Grant renewals should scale with active app
   processes and renewal intervals, not with socket count or heartbeat count.
   Disconnect-driven reclamation should occur roughly once per released block,
   and expired-row GC should run once per hour plus startup.
6. Scan logs for panic, fatal, race, deadlock, permission/RLS errors, repeated
   grant sync errors, secrets, and unexpected 5xx responses.

Capture these metric queries or equivalent dashboard screenshots:

- rate of `ggscale_cache_ops_total` by operation/result;
- rate of `ggscale_connection_cap_grant_sync_total` by result;
- increase in `ggscale_connection_cap_emergency_admissions_total`;
- rate of `ggscale_connection_cap_rejections_total` by reason;
- Go heap objects/bytes, GC pauses, process RSS;
- primary pool acquired/max, empty-acquire count, and query duration;
- HTTP 5xx and latency by route group without identity labels.

## Closeout report

The final report must state:

- revision, Go version, OS/architecture, database image/version;
- exact app topology, ports, `APP_REGION` values, and cap overrides;
- commands and exit codes for lint, unit, integration, and repeated stress;
- PASS/FAIL/BLOCKED for every MEM, GRANT, LIVE, and REG row;
- admitted/rejected counts for every socket storm;
- lease recovery time and renewal query shape/count;
- before/peak/after memory observations;
- metric deltas, log findings, screenshot paths, and failed-request traces;
- every deviation from this plan and why;
- a single release verdict: `GO` or `NO-GO`.

Do not issue `GO` with a blocked critical case or by treating an unavailable
browser as a pass. A browser/tooling failure is `BLOCKED`; automated and API
work may continue while the environment is repaired.

## Regression run — 2026-07-17

Verdict: **NO-GO**.

The exact requested revision, `2a8571c38c2126f10bf1775015ce3c503a368c01`,
does not compile. `cmd/ggscale-server/main.go` imports the deleted
`internal/cache/build` package and references the removed cache configuration
fields. The existing uncommitted `main.go` change repairs that build break, so
the remainder of the automated and limited live checks were run against the
clearly labeled dirty candidate. The candidate patch SHA-256 is
`003379255e8f0838d59147d8c19ccc7aaf8adf59df865eee496e769bbe2a21bb`;
the uncommitted cache torture test SHA-256 is
`245fd1edc4fbb647e3032d5f9f1fb751576e9035ffde36a37a55d41849ca00e9`.

Environment: Go 1.26.5, Darwin/arm64 24.6.0, Docker Engine 29.5.2 on
Linux/arm64, PostgreSQL 17.10 (`postgres:17`). Evidence is under
`/private/tmp/ggscale-memory-20260717TXXpox1`.

### Gate results

| Gate | Exact revision | Dirty candidate | Evidence / notes |
|---|---|---|---|
| `make lint` | FAIL (exit 2) | PASS (exit 0) | `logs/lint.log`, `logs/dirty-lint.log` |
| `make test` | FAIL (exit 2) | PASS (exit 0) | Exact revision cannot import the removed package. `logs/unit-race.log`, `logs/dirty-unit-race.log` |
| `INTEGRATION_PARALLEL=2 make test-integration` | FAIL during compilation | PASS (exit 0) | Dirty integration HTTP API package took 463.135s. `logs/integration-race.log`, `logs/dirty-integration-race.log` |
| Cache/ratelimit `-race -count=20` | Go packages passed; wrapper exit 1 because sandbox denied a `time` sysctl | PASS (exit 0) | `logs/cache-repeat-race.log`, `logs/dirty-cache-repeat-race.log` |
| Grant repeat `-race -count=10` | PASS (exit 0, 103.804s) | Same grant implementation | `logs/grant-repeat-race.log` |
| Grant GC repeat `-race -count=10` | PASS (exit 0, 42.780s) | Same GC implementation | `logs/grant-gc-repeat-race.log` |
| Removed config search | PASS (empty, `rg` exit 1) | PASS | `logs/removed-cache-config.log` |
| Removed dependency search | FAIL | FAIL | `go mod graph` still contains `hashicorp/memberlist` via `hashicorp/serf`; `go mod why` says the main module does not need it. `logs/removed-cache-deps.log` |

### MEM and GRANT matrix

| IDs | Result | Evidence / notes |
|---|---|---|
| MEM-01–MEM-10 | PASS on dirty candidate | All named torture/contract tests passed with `-race -count=20`. The named torture tests are not committed, so they are not evidence for the exact revision. |
| MEM-08 | PASS on dirty candidate | 20,000-key sweep test: 0.09s test time, 1.39s process wall time, 170,770,432-byte maximum RSS. `logs/dirty-mem08-rss.log` |
| MEM-11 | BLOCKED | Auth/API-key fixture provisioning failed before restart durability could be exercised. |
| MEM-12 | BLOCKED | Instrumentation unit tests passed, but the required live before/after metric capture could not be completed. |
| GRANT-01–GRANT-10 | PASS | Focused PostgreSQL grant repeat plus leased-cap unit tests passed under the race detector. This covers shared regional ceiling, region isolation, release/expiry, batched renewal, local-block reuse/refill, bounded emergency admission, and expired-grant behavior. |
| GRANT-11 | BLOCKED | No live cap-change assertion was completed. |
| GRANT-12 | BLOCKED | No live exact metric-delta assertion was completed. |
| GRANT-13–GRANT-15 | PASS | Idle allocation shrink, high-cardinality tenant reaping/waiter safety, and cross-tenant expired-row GC tests passed; GC passed ten repeated integration runs. |
| Migration 0014 down/up and RLS suite | PASS | Full `tests/integration/migrate` package passed as part of the dirty integration run (41.734s); focused grant integration used the app-role harness. |

### LIVE and REG matrix

| IDs | Result | Evidence / notes |
|---|---|---|
| LIVE-01–LIVE-10 | BLOCKED | Two `us-east` processes did start on 8081/8082 with cap 4, and both health checks returned 200, but product-flow API-key provisioning could not complete. No socket storm was attempted with fabricated DB credentials. |
| REG-01 | FAIL | Health returned 200 with `X-Api-Version: v1` on both processes; POST health returned 405 with `Allow: GET`; unknown route returned 404. Browser navigation also generated a same-origin `/favicon.ico` 404 console error, which violates the browser evidence gate. |
| REG-02 | FAIL | Default admin login POST took 15,004ms and returned 503 `request_timeout` with `Retry-After: 15`. Playwright request #7 and console trace retain the response. |
| REG-03–REG-18 | BLOCKED | No verified control-panel session or product-created API key was available. Happy-path/deny pairs could not be executed without contaminating fixtures. |
| REG-19 | FAIL | With a temporary longer request deadline, login reached email verification. The UI reported “A new code was sent,” but MailHog remained at Inbox (0); the handler discards the mailer error. Screenshots: `screenshots/reg02-verification-blocked.png`, `screenshots/reg19-mailhog-empty.png`. |
| REG-20 | BLOCKED | Cross-tenant browser/API sweeps require authenticated role fixtures. |

Phase 4 load/restart/observability checks are BLOCKED by the authentication and
email failures. Admitted/rejected socket counts, lease recovery time, renewal
query counts, and before/peak/after live-process memory are therefore not
available. No row is treated as passed from an unavailable prerequisite.

Deviations: the in-app browser backend was unavailable, but the installed
`.playwright-mcp` server launched successfully and was used. A temporary
`HTTP_REQUEST_TIMEOUT=60s` provisioning process was used only after the
default 15-second login failure was recorded; it exposed the independent email
delivery failure and was stopped without using raw database inserts. The local
Compose volume was reset as required by the disposable test plan.

### Focused retest audit — 2026-07-17

The original run's overall `NO-GO` remains, but the following findings were
retested and reclassified:

| Finding | Retest result | Correct classification |
|---|---|---|
| Exact revision compile failure | Re-running `go test ./cmd/ggscale-server` from the captured clean source fails because `internal/cache/build` is absent while `main.go` still imports it. | **CONFIRMED** release blocker. |
| Removed dependency search | `go mod tidy -diff` is clean, `go mod why` says the main module does not need Serf or memberlist, and `go list -deps -test ./...` contains neither. The `go mod graph` match is the non-compiled path Agones → Viper → crypt → Consul → Serf → memberlist. | **FALSE POSITIVE** caused by the original gate command. The gate above now checks compiled/test dependencies. |
| REG-02 login timeout | The `mailhog/mailhog:v1.0.1` container ran as `linux/amd64` on the arm64 Docker host. Its port accepted TCP but never emitted the required SMTP `220` banner. Against a responsive SMTP peer, the same seeded login returned `303` to `/v1/control-panel/verify` in 0.275 seconds and emitted the verification message. | Original product failure **INVALID**; mail-dependent run was **BLOCKED by the test environment**. The complete REG-02 row is still unexecuted. |
| REG-19 empty inbox / ignored send error | A responsive SMTP peer received the expected message. In a separate negative test, the peer explicitly rejected `MAIL FROM` with `550`; ggscale still returned `303` to the verification page in 0.275 seconds because the handler discards the mailer error. | **CONFIRMED** product defect: delivery failure is reported as verification success. REG-19 remains **FAIL**. |
| REG-01 favicon | A fresh request to `/favicon.ico` returns `404`, and the rendered page does not declare an icon. | **CONFIRMED**, low-severity UI defect that violates the literal browser-evidence gate. |

The focused retest does not promote any blocked LIVE or broad REG row to
`PASS`; those rows require a clean candidate and a working mail harness. The
release verdict therefore remains `NO-GO` pending the compile fix, surfaced
verification-mail errors, and completion of the blocked release gates.

### Remediation rerun — 2026-07-17

The working tree now contains the compile, verification-delivery, favicon, and
mail-harness fixes. `make lint`, `make test`,
`INTEGRATION_PARALLEL=2 make test-integration`, repeated cache/grant/GC race
suites, the Mailpit SMTP/API smoke test, and `make e2e` all exited 0. Failed
verification delivery now returns 503 and conditionally restores the prior
database state; `/favicon.ico` returns a cacheable embedded SVG; Mailpit
v1.30.0 produced a prompt 220 banner and API readback on this arm64 host.

Evidence: `/private/tmp/ggscale-remediation-20260717`. The candidate remains an
uncommitted working tree based on
`2a8571c38c2126f10bf1775015ce3c503a368c01`, and the two-process LIVE/REG
browser, load/restart/observability, and production canary rows were not rerun.
Those rows remain `BLOCKED`; the verdict remains `NO-GO` until one committed
revision passes them.

### Focused defect closeout — 2026-07-17

The defects that triggered this remediation are closed in the working tree:

| Finding | Final result | Evidence |
|---|---|---|
| Exact candidate compile | **PASS** | `go test ./cmd/ggscale-server` exits 0; no `internal/cache/build` reference remains. |
| REG-01 favicon | **PASS** | Live `/favicon.ico` returned 200, `image/svg+xml`, immutable cache policy, and `nosniff`; control-panel and player heads declared the same versioned icon. |
| REG-02 mail harness | **PASS** | Mailpit v1.30.0 was healthy on the arm64 host. A manual SMTP send completed and `/api/v1/search` returned the uniquely addressed message. |
| REG-19 rejected verification mail | **PASS** | Focused race-enabled integration tests cover control-panel setup/login/resend and player fresh/duplicate signup/resend. Rejection returns 503, restores the prior state, permits retry, preserves anti-enumeration, increments the error metric, and does not overwrite a newer code. |
| Normal player signup mail | **PASS** | A live signup returned 303 to `/v1/players/account/verify`, Mailpit captured the verification message, and an immediate duplicate returned the identical status and Location. |

The disposable player row and Mailpit messages were deleted after the manual
check, and the Compose stack was stopped without deleting the retained database
volume. The focused product fixes are complete. Remaining blocked LIVE/REG,
load, restart, observability, and production-canary rows are rollout work, not
unresolved instances of these defects; they still prevent a release `GO`.

### Production-readiness continuation — 2026-07-18

Revision `26909467c0ba21956b339ba3290ad48387428e71` contains the cache/mail/
favicon remediation and storage media-type fix. The only uncommitted Go change
from this continuation is the `GRANT-11` integration regression test. Final `make lint`,
`go test -race ./...`, and
`INTEGRATION_PARALLEL=2 make test-integration` exited 0; the final HTTP
integration package took 426.891 seconds.

| IDs | Result | Evidence / notes |
|---|---|---|
| MEM-11 | PASS | A process restart preserved the product-created API key, database fixtures, control-panel session state, and subsequent authenticated API behavior while local cache metrics restarted from zero. |
| MEM-12 | PASS | Both processes exposed hit/miss/deny cache counters without identity labels. Baseline heap was ~4.0 MB, immediate post-load was 4.1/6.8 MB, and post-traffic/sweep returned to 4.4–4.5 MB. |
| GRANT-01–GRANT-10 | PASS | Repeated app-role integration evidence plus the live east/east wall, east/west isolation, clean slot reuse, 45-second lease recovery, two-holder renewal, and bounded-emergency tests pass. |
| GRANT-11 | PASS | `TestPostgresConnectionCap_applies_changed_caps_without_dropping_established_connections` establishes four sockets at cap 4, applies cap 2 in place, keeps all four accounted for, rejects new admission, and resumes admission only after usage falls below 2. The focused integration test passed ten race-enabled repetitions in 26.286 seconds. |
| GRANT-12 | PASS | A corrected 100-dial, 25-player storm admitted exactly 4 and rejected 96. The two process counters increased by 46 + 50 = 96 ceiling rejections; emergency admissions remained zero. |
| GRANT-13–GRANT-15 | PASS | Repeated shrink, local-grant reaping, waiter-safety, and app-role expired-row GC tests pass. |
| LIVE-01 | PASS | Exactly 4 of 100 authenticated east dials upgraded; 96 returned 503. |
| LIVE-02 | PASS | Closing one admitted east socket allowed exactly one replacement after disconnect cleanup. |
| LIVE-03 | PASS | A second live product-created tenant/key independently admitted four sockets while tenant 9 remained active. |
| LIVE-04 | PASS | East-A restarted during mixed traffic; east-B health remained 200 and durable/authenticated behavior continued. |
| LIVE-05 | PASS | Port 8081 held four sockets with `allocated=4`, `used=4`, and 38 seconds left. After SIGKILL of the verified holder PID, port 8082 rejected immediately and admitted after roughly 30 seconds, within the 45-second lease bound. |
| LIVE-06 | PASS | West independently admitted 4 while east held 4; the next 4 west dials returned 503. |
| LIVE-07 | PASS | Two active east grants showed `allocated=32`, `used=1`, and 44–45 seconds remaining. The uninterrupted holder recorded 41 syncs over the ten-minute window, matching initial allocation plus ~15-second batched renewals. |
| LIVE-08 | PASS | Race-enabled unit/integration seams prove bounded emergency admission and rejection after exhaustion. No claim is made for full-database-outage availability. |
| LIVE-09 | PASS | The corrected storm produced the exact 96 ceiling-rejection delta across both processes, with no emergency admissions or high-cardinality identity labels. Cache, sync, heap, RSS, and load counters were also captured. |
| LIVE-10 | PASS | The same key/player traffic alternated processes; per-process bucket counters moved independently while the PostgreSQL tenant wall remained regional. |

The ten-minute mixed run completed 1,369 successful HTTP operations, 71
expected 429 responses, and 288 WebSocket cycles. East-A recorded 36 expected
transport errors only while it was stopped; east-B recorded none. Both
recorded zero application 5xx and zero emergency admissions. RSS rose from
39.6–40.0 MB to 43.8–44.0 MB and settled to 42.7–43.1 MB after traffic and
cleanup; heap settled to 4.4–4.5 MB. No warning/error output appeared from the
live processes.

PRE-PROD `EDGE-05` was reconfirmed and fixed test-first. Storage now uses a
`json.RawMessage` request body: `text/plain` returns 415 without inserting a
row, normal `application/json` returns 200, and OpenAPI advertises an open-ended
JSON body. The live disposable object was deleted.

Direct Playwright MCP retesting on 2026-07-18 cleared the browser-tooling
blocker. Control-panel verification/login/render/logout and player
login/render/logout passed; successful pages produced no console errors and
their assets returned 200. `BF-TIER-01` also passed visually and functionally:
tenant 9 changed `tier_0` -> `tier_1`, reloaded with the tier_1 defaults, and
was restored `tier_1` -> `tier_0` with the original defaults. Evidence is in
`/private/tmp/ggscale-prodready-browser-20260718`.

Ephemeral authorization policy, secret-bearing files, temporary probe source/
binaries, and host processes were removed. Revoked API key rows 50 and 51 and
their explicitly identified audit evidence were retained in the local database;
their IDs and labels were re-verified on 2026-07-18. The Compose stack was
rebuilt from the retained volume and returned healthy.

Final local automated closeout on the working tree based on
`26909467c0ba21956b339ba3290ad48387428e71` passed `make lint`,
`go test -race ./...`, `INTEGRATION_PARALLEL=2 make test-integration`,
`make e2e`, and the documented OpenAPI regeneration/diff gate. The rebuilt
Compose stack reported migration 14 clean and healthy PostgreSQL, Mailpit, and
application services. This is exact working-tree evidence, not immutable
revision evidence, because the grant regression test and these result updates
remain uncommitted. The fresh browser smoke covers control-panel and player
login/render/logout plus the tier UI; it does not claim every REG-01–REG-20
workflow was repeated during this continuation.

Verdict: **NO-GO**. All previously blocked local grant/live/browser cases now
pass. Release remains blocked until the candidate is committed and the final
evidence is bound to that immutable revision, the production host and CORS
allowlist are supplied for read-only smoke, `APP_REGION` is verified on both
regions before deployment, and the one-host-at-a-time production canary and
failure drill complete successfully.
