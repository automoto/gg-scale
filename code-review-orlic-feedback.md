# Olric Refactor — Code Review Feedback

High-effort review of the Olric caching/rate-limit refactor plus the storage-limit
and CI changes on this branch (`git diff HEAD~1`, 26 files). Ten distinct defects
survived independent verification. This document is a fix handoff for a coding agent.

**Root-cause note:** Findings 1, 5, 6, 7, 9, 10 all trace to a single design change —
`AcquireSlot`/`ReleaseSlot`/`RefreshSlot` were converted from lock-free atomic
`Incr`/`Decr`/`Expire` into a serialized distributed read-modify-write behind
`withSlotLock` → `withDistributedLock` → `burstLocks.LockWithTimeout`. Before fixing
these one by one, evaluate whether that conversion is needed at all. Reverting to
atomic `Incr`/`Decr`/`Expire` for the slot counter clears six of the ten findings.
Fix findings 2, 3, 4, 8 independently regardless of that decision.

Severity legend: **High** = release blocker / player-facing outage / security.
**Medium** = availability degradation under load or misconfiguration. **Low** =
efficiency or narrow-trigger correctness.

---

## HIGH

### H1 — Permanent player lockout on a leaked ReleaseSlot
- **File:** `internal/cache/olric/olric.go:438` (`ReleaseSlot`)
- **Problem:** `ReleaseSlot` now wraps its `Decr`/`Delete` in `withSlotLock`.
  `withDistributedLock` calls `burstLocks.LockWithTimeout(ctx, key, burstLockLease,
  burstLockDeadline)` (30s deadline) and returns the error **before** `fn()` runs, so
  on a lock timeout the counter is never decremented. The realtime deferred release
  (`internal/realtime/server.go`, called with `context.Background()`) only logs the
  error as `Warn`. After `MaxPerPlayer` such leaks the player is rejected with
  "too many connections for this user" on every connect until the slot TTL expires.
- **Fix direction:** Make decrement not depend on acquiring a lock that can time out.
  Prefer restoring the lock-free atomic `Decr` path. If the distributed lock is kept,
  the release path must never leave the counter un-decremented — e.g. tolerate lock
  failure and still `Decr`, or make release idempotent/self-healing. Do not rely on
  TTL expiry as the recovery mechanism.
- **Verify:** Add a test where the lock acquisition fails/times out and assert the
  slot counter is still decremented (player can reconnect).

### H2 — Per-IP rate limits multiply by cluster size
- **Files:** `internal/cache/build/build.go:64`, `internal/cache/build/split.go:24`
- **Problem:** `store = newSplitStore(memory.New(), s)` routes `TokenBucket` to a
  per-process `memory.New()` store instead of the clustered Olric `ratelimit` DMap.
  Behind the load balancer with N app nodes, a client's request budget is enforced
  per-node, so it can sustain up to **N× the configured rate** before any node
  rejects. The DoS/rate-limit invariant is silently broken by cluster size.
- **Fix direction:** Route `TokenBucket` back through the clustered store when the
  backend is Olric. If the split was intentional for latency reasons, that trade-off
  must be explicit and documented, and the rate limit must remain globally correct
  across nodes (the whole point of the shared `ratelimit` DMap). Confirm what problem
  the split was solving before choosing.
- **Verify:** Test that two `splitStore` instances sharing one backing store enforce a
  single combined token budget for the same key.

### H3 — govulncheck CI gate narrowed to symbol-level only
- **File:** `scripts/govulncheck.sh:150`
- **Problem:** The old gate failed on any finding with `.trace | length > 0`
  (module, package, or symbol level). The new script classifies findings and only
  feeds `symbol_findings` (frames with a `.function`) into `unaccepted` / `exit 1`;
  package- and module-level reachable findings are printed as informational and pass.
  A vulnerability govulncheck reports at package/module precision now ships green.
- **Fix direction:** Restore a fail-closed gate: any unaccepted reachable finding at
  **any** level should fail CI. Keep the accept-list mechanism for explicitly
  triaged OSVs, but do not silently downgrade package/module-level findings to
  informational. If tiered handling is genuinely wanted, package/module findings must
  still be able to fail the build unless explicitly accepted.
- **Verify:** Update / add `scripts/govulncheck_test.sh` cases covering a
  package-level and a module-level finding — both must exit non-zero when unaccepted.

### H4 — Per-tenant storage override above platform default silently 413s
- **File:** `internal/httpapi/storage.go:44` (`storageBodyReadLimit` / `storageLimit`),
  also enforced at `:107` (route registration `MaxBodyBytes`)
- **Problem:** Huma's `MaxBodyBytes` is computed from the static platform default
  (`storageLimit(d)` → `d.StorageMaxValueBytes`), fixed at route registration. The
  handler enforces the per-tenant/project value from `resolveStorageLimit`
  (`d.StorageLimits.Resolve`), which can be **larger** than the platform default. A
  body within the tenant override but above the default passes the handler's
  `len(in.RawBody) > limit` check yet is rejected by Huma first with 413
  `RequestEntityTooLarge`. The override feature this change advertises is broken for
  any override exceeding the server-wide default. The integration test passes only
  because it sets `StorageMaxValueBytes` equal to the override on that server.
- **Fix direction:** `MaxBodyBytes` must be an upper bound over the largest possible
  resolved limit, not the platform default — e.g. register with the max of the
  default and any configured tenant/project override ceiling, or read the raw body
  and enforce the resolved per-request limit inside the handler instead of relying on
  Huma's static cap. Keep a sane absolute ceiling to bound memory.
- **Verify:** Integration test with `StorageMaxValueBytes` = 1 MiB, a tenant override
  = 4 MiB, and a 2 MiB PUT — assert it succeeds (not 413).

---

## MEDIUM

### M5 — 30s head-of-line blocking on unrelated keys
- **File:** `internal/cache/olric/olric.go:361` (`withDistributedLock`)
- **Problem:** Every key is funneled through a fixed 64-entry local mutex table
  (`localLocks[maphash.String(seed, key) % 64]`). The chosen local mutex is held for
  the **entire** distributed-lock acquisition (up to `burstLockDeadline` = 30s). Two
  unrelated keys colliding in the same bucket block each other even when both Olric
  locks are free — player-join/rate-limit requests hang up to 30s with no real
  contention.
- **Fix direction:** Don't hold a shared local mutex across a blocking network lock
  acquisition. If a local mutex is needed only to dedupe same-key work, key it per
  distinct key (not a 64-bucket hash) or drop it. Best resolved by the root-cause
  revert (H-note above).

### M6 — Acquire deadline raised 5s → 30s turns sub-ms calls into 30s hangs
- **File:** `internal/cache/olric/olric.go:339` (`burstLockDeadline`), also `:287`
  (`AcquireSlot`)
- **Problem:** `AcquireSlot` dropped the lock-free `slots.Incr` fast path for a
  serialized read-modify-write behind `burstLocks.LockWithTimeout` with the deadline
  raised from 5s to 30s. On a hot slot/matchmaking key under a spike, callers queue
  and an unlucky one blocks up to 30s before returning, tying up HTTP goroutines.
- **Fix direction:** Restore the lock-free `Incr` path (root-cause revert), or if the
  lock is kept, choose a deadline that fails fast (single-digit seconds) so callers
  don't pile up. Justify any deadline > the request timeout.

### M7 — Crashed lock holder strands per-key lock for 60s (was 15s)
- **File:** `internal/cache/olric/olric.go:338` (`burstLockLease`)
- **Problem:** `burstLockLease` raised 15s → 60s. If the node holding a burst/slot
  lock dies mid-critical-section, every acquire/release for that key blocks or fails
  for up to a full minute (was 15s) — 4× the per-key outage window on node failure.
- **Fix direction:** Return the lease to a short value (~15s or less) sized to the
  actual critical-section duration. The lease is the failure-recovery bound; it
  should be as short as the work allows. Superseded if the root-cause revert removes
  the lock entirely.

### M8 — Loopback bind hostname bypasses the routable-address guard
- **File:** `internal/config/validate.go:346` (`checkOlricNetwork`), helper
  `isLoopbackOrLinkLocal` at `validate.go:16`
- **Problem:** `isLoopbackOrLinkLocal` does `net.ParseIP(s); if ip == nil { return
  false }`, so a hostname like `localhost` returns `false` and the guard passes. With
  `CACHE_BACKEND=olric`, `CACHE_OLRIC_PEERS` set, and `CACHE_OLRIC_BIND_ADDR=localhost`,
  Olric binds/advertises a loopback-resolving data-plane address, peers can't reach
  the node, and it silently forms a broken single-node cluster instead of failing
  loud at boot as the guard intends.
- **Fix direction:** Resolve the bind address before classifying it — if it's a
  hostname, resolve it (or at minimum reject the well-known loopback names) and apply
  the same loopback/link-local/unspecified check to the resolved IP(s). Fail loud at
  boot when a multi-peer Olric config binds a non-routable address.
- **Verify:** Table test asserting `CACHE_OLRIC_BIND_ADDR=localhost` (and `127.0.0.1`)
  both fail validation when peers are configured.

---

## LOW

### L9 — RefreshSlot heartbeat does 4 round trips instead of one Expire
- **File:** `internal/cache/olric/olric.go:463` (`RefreshSlot`)
- **Problem:** `RefreshSlot` now does local-mutex + distributed `Lock` + `Get` + `Put`
  + `Unlock` (4 network round trips to the partition owner) instead of a single atomic
  `Expire`. On a hot keepalive path this multiplies cache traffic and lock contention
  on the partition owner under connection load and raises heartbeat latency.
  Correctness is intact; this is an efficiency regression.
- **Fix direction:** Restore the single atomic `Expire` for TTL refresh (root-cause
  revert). Refresh does not need read-modify-write.

### L10 — Saturated slot counter can expire and briefly over-admit (PLAUSIBLE)
- **File:** `internal/cache/olric/olric.go:299` (`AcquireSlot` reject path)
- **Problem:** The old reject path ran `slots.Expire(ctx, key, ttl)` before returning
  `false`, keeping a saturated counter alive. The new reject path returns
  `current = limit` and writes nothing. If a slot key is saturated and traffic is
  mostly rejected acquires (holders not refreshing within `ttl`), the counter's TTL
  elapses, the key expires and resets to 0, and the next acquires are admitted —
  briefly allowing more concurrent slots than `limit`. Marked PLAUSIBLE (narrow
  trigger), not CONFIRMED.
- **Fix direction:** Refresh the key's TTL on the reject path (restore the `Expire`
  call), tolerating `ErrKeyNotFound`. Folds into the root-cause revert.

---

## Refuted during verification (do not action)
- Removed `isKeyNotFound` string-fallback for Olric not-found errors — the remaining
  `errors.Is(err, olricpkg.ErrKeyNotFound)` path on `Get` is sufficient.
- `NewClusterClient` dialing `127.0.0.1:0` when `BindPort == 0` — `Sanitize()` coerces
  a zero `BindPort` to `DefaultPort`, so it is never 0.

---

## Suggested fix order
1. **H2, H3** — security gates; smallest blast radius, highest severity.
2. **H1, H4** — player-facing correctness.
3. Decide the root-cause question (revert slot locking to atomics vs. keep the lock).
   If reverting: M5, M6, M7, L9, L10 resolve together.
4. **M8** — independent config-validation fix.

Run `make lint` and `make test` after each change. Do not commit — leave changes
staged for review.
