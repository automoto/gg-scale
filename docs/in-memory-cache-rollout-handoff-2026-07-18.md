# In-memory cache rollout handoff — 2026-07-18

## Current state

- Repository: `/Users/mydev/code/ggscale`
- Current revision: `2690946` (`main`, matches `origin/main`)
- Local Compose stack is still running on port 8080 with healthy PostgreSQL
  and Mailpit. The temporary host processes on ports 8081 and 8082 are stopped.
- Tenant 9 was restored to `tier_0` after the browser tier test.
- The storage `application/json` fix and the browser-result documentation are
  present in the current revision.
- One useful new integration test is uncommitted:
  `TestPostgresConnectionCap_applies_changed_caps_without_dropping_established_connections`
  in `internal/ratelimit/postgres_connection_cap_integration_test.go`.
- `infra` is dirty independently of this work; do not modify or clean it as
  part of the cache rollout.

## Verified tonight

### Browser checks — PASS

Direct Playwright MCP was used against the rebuilt Compose server.

- Control-panel verification through Mailpit, login, authenticated rendering,
  and logout passed. Required assets returned 200 and the successful flow had
  no console errors.
- Player login, account rendering, and logout passed. The successful flow had
  no console errors.
- `BF-TIER-01` passed visually and functionally. The platform-admin selector
  changed tenant 9 from `tier_0` to `tier_1`, the reloaded page showed tier_1
  and its 1000/2000 API defaults, and the selector restored the tenant to
  `tier_0` with the original 150/300 defaults.
- Screenshots are retained in
  `/private/tmp/ggscale-prodready-browser-20260718`.

### Regional connection-cap checks — PASS

- Corrected multi-player storm: 100 simultaneous WebSocket dials used 25
  distinct product players across the two east processes. Exactly 4 upgraded
  and 96 returned 503.
- Exact metrics: the two processes reported 46 + 50 = 96
  `ggscale_connection_cap_rejections_total{reason="ceiling"}` increments.
  Emergency admissions stayed at zero. This clears `GRANT-12` and `LIVE-09`.
- Second live tenant: a separately created tenant-10 key admitted four sockets
  while tenant 9 was active. This clears `LIVE-03`.
- Exact SIGKILL drill: port 8081 held four connections with a database grant
  showing `allocated=4`, `used=4`, and 38 seconds left. After SIGKILL of the
  confirmed port-8081 PID, a fresh-token probe on port 8082 immediately
  returned 503, then admitted after roughly 30 seconds, inside the 45-second
  lease requirement. This clears `LIVE-05` and `BF-WS-04`.
- In-place cap change: the new PostgreSQL integration test establishes four
  connections at cap 4, applies cap 2 on the next synchronization, proves the
  existing four remain accounted for and new admissions stop, then proves
  admission resumes only after usage falls below 2. Focused race-enabled test:
  `ok github.com/ggscale/ggscale/internal/ratelimit 3.212s`. This clears
  `GRANT-11`.

The temporary WebSocket probe source/binary, login responses, email lists, and
metric captures are under `/private/tmp/ggscale-prodready-*`; none are in the
repository.

## Cleanup still needed

Two product-created API keys were revoked through the control panel but remain
as revoked local rows:

- API key 50 — tenant 9, project 25, `prodready-live-tenant9`
- API key 51 — tenant 10, project 26, `prodready-live-tenant10`

Before deleting them, re-verify the IDs and labels. Their matching local
platform-audit rows are 13–16. The load probe also created `auth.login` audit
rows 131–180. Platform-audit rows 8–12 record tonight's browser login/logout
and tier-change/restore checks. These are local test artifacts; decide whether
to preserve the audit evidence or delete the explicitly identified rows with
the fixture keys. Do not use a broad time-range delete.

The platform-admin seed is now email-verified because the browser smoke
completed its real Mailpit verification flow.

## Remaining test and rollout cases

1. Run `gofmt`/focused test once more if the new grant test is edited, then run
   `make lint`, `go test -race ./...`, and
   `INTEGRATION_PARALLEL=2 make test-integration` on the exact final revision.
   The full suite passed before the new test was added; the new focused test
   itself passes.
2. Reconcile the final result tables in
   `docs/in-memory-cache-rollout.md`,
   `docs/IN_MEMORY_CACHE_AND_REGRESSION_TEST_PLAN.md`, and
   `docs/PRE_PROD_TEST_PLAN.md`: mark `GRANT-11`, `GRANT-12`, `LIVE-03`,
   `LIVE-05`, `LIVE-09`, and `BF-WS-04` PASS using the evidence above.
3. Complete the plan's exact-final-revision REG closeout. The current candidate
   has fresh control-panel/player browser smoke and the formerly blocked tier
   UI case; do not imply that every REG-01–REG-20 workflow was repeated tonight.
4. Commit the new integration test and any final documentation update, then
   bind the final lint/race/integration evidence to that immutable revision.
5. Production operations remain external and are still release-blocking:
   set `APP_REGION` correctly on east and west through bw-ops **before** the app
   deploy; deploy one host so migration 0014 runs; verify migration state,
   health, grant sync, zero emergency admissions, and no new 5xx; perform the
   one-host-at-a-time canary/failure drill; then repeat in the other region.
6. Production read-only smoke still needs the real production host and expected
   CORS allowlist. Do not run destructive tier, key, account, or socket-storm
   tests against production.

## Release verdict

All previously outstanding local grant/browser bugs now have passing evidence.
The formal verdict remains **NO-GO** only until cleanup, exact-final-revision
REG/full-suite closeout, production `APP_REGION` verification, and the staged
production canary are complete.
