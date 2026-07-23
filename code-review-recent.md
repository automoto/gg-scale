# Production Code Review

## Scope

The initial 85K-line figure was a raw count:

- 34,351 lines of authored production Go
- 26,216 lines of tests
- 24,941 lines of generated Go
- 85,508 total lines

This review covered the 34.4K authored production lines, plus SQL, migrations, CI, and deployment configuration.

## Production verdict

Do not launch as-is. No confirmed critical issue was found, but eight high-severity findings should be addressed before production.

## Critical

None confirmed.

## High

1. **Refresh tokens can be rotated successfully more than once.** Two concurrent requests can both read the session as active, one silently updates zero rows, and both insert valid successor sessions. This defeats single-use token rotation and reuse detection. Use `SELECT … FOR UPDATE` or an atomic `UPDATE … RETURNING`, issuing a successor only to the winner. [auth.go](internal/httpapi/auth.go#L576), [auth.sql](internal/db/queries/auth.sql#L122)

2. **Storage listing can allocate and buffer roughly 100 MiB per request.** The endpoint returns up to 100 complete 1 MiB values; PostgreSQL materializes them, Go stores them, JSON encoding copies them, and the deadline middleware buffers the entire response again. A normal authenticated player can use this as a memory-exhaustion vector. Make lists metadata-only or impose an aggregate response cap and bounded writer. [storage.go](internal/httpapi/storage.go#L23), [storage.go](internal/httpapi/storage.go#L323), [storage.sql](internal/db/queries/storage.sql#L121), [deadline.go](internal/middleware/deadline.go#L41)

3. **Tenant admins can mutate or revoke secret API keys.** Creation performs a key-type authorization check, but relabel, scope-update, and revoke handlers rely only on the generic `project:manage` route guard. That lets an admin disrupt backend/game-server credentials or alter their scopes despite policy granting admins access only to publishable keys. Load the target key type and authorize every mutation against `api_key:secret` or `api_key:publishable`. [handler.go](internal/controlpanel/handler.go#L184), [handler.go](internal/controlpanel/handler.go#L647), [policy.go](internal/rbac/policy.go#L40)

4. **WebSocket connections are not reaped when heartbeat fails, and inbound traffic bypasses rate limits.** The heartbeat goroutine can finish while the main goroutine remains blocked forever in `Read`. Every ignored inbound message also performs slot refreshes using an unbounded background context. A player can retain dead connections or flood cache/CPU work. Close or cancel the connection when heartbeat fails and reject or rate-limit unused inbound messages. [server.go](internal/realtime/server.go#L182)

5. **Polling-only matchmaking returns a dead game server.** After tickets are committed, the allocation is immediately destroyed when no WebSocket notification was delivered, even though the comment says players may recover by polling. The poll result consequently points at a server that was just deallocated. Keep the allocation alive through a lease or explicit acknowledgement timeout. [worker.go](internal/matchmaker/worker.go#L516)

6. **A successful fleet allocation is orphaned when `MarkReady` fails.** Once Docker or Agones creates the resource, a database failure returns without deallocating it or recording a usable backend reference. This leaks containers or GameServers and potentially ongoing cost. Attempt immediate cleanup and persist enough state for reconciliation if cleanup fails. [manager.go](internal/fleet/manager.go#L133)

7. **The runtime retains migration-owner database credentials.** The same `DATABASE_URL` runs migrations and creates the application pool. `SET ROLE ggscale_app` does not remove the original session user's privileges; `RESET ROLE` restores them. The startup assertion only inspects `current_user`. Use separate migration and runtime credentials, with the runtime login unable to alter schema, bypass RLS, or assume the migration role. [main.go](cmd/ggscale-server/main.go#L163), [main.go](cmd/ggscale-server/main.go#L529)

8. **Conditional: the Docker backend is unsafe for tenant-controlled images.** Production requires digest pinning but permits an empty registry allowlist, provides unrestricted default container networking, and compares allowlist entries with `HasPrefix`; `ghcr.io/acme` therefore permits `ghcr.io/acme-evil/...`. If customers can configure fleets, require an exact parsed repository allowlist and isolate egress, internal networks, and the Docker daemon. [validate.go](internal/config/validate.go#L150), [backend.go](internal/fleet/docker/backend.go#L180), [backend.go](internal/fleet/docker/backend.go#L233)

## Medium

1. **The distributed Olric token bucket is deliberately non-atomic.** Parallel requests can all read the same balance, all be admitted, and overwrite one another's debits. This makes clustered auth and API rate limits bypassable with concurrency. Use a per-key distributed lock or atomic server-side transition. [olric.go](internal/cache/olric/olric.go#L167)

2. **Email-verification cookies use random per-process signing keys.** Restarts invalidate active flows, and multi-replica deployments fail intermittently unless sessions are sticky. Use a shared versioned key from secret storage. This affects both player and control-panel verification. [players.go](internal/players/players.go#L91), [handler.go](internal/controlpanel/handler.go#L121)

3. **CI omits the integration, E2E, and vulnerability suites.** CI only runs lint and unit/race tests; the 44 build-tagged integration files covering migrations, RLS, authentication, and tenant isolation are not release gates. [ci.yml](.github/workflows/ci.yml#L40), [Makefile](Makefile#L40)

4. **The vulnerability wrapper produces unreliable results.** It swallows all scanner failures, so an offline database lookup reported "passed"; it also treats package/module import traces as reachable-symbol findings. With network access, the actionable finding is that Go 1.26.4 reaches GO-2026-5856, fixed in 1.26.5. Upgrade `go.mod`, Docker, and CI to 1.26.5 and make the wrapper distinguish scan errors, symbol findings, package findings, and module findings. [govulncheck.sh](scripts/govulncheck.sh#L69), [go.mod](go.mod#L3), [official Go 1.26.5 release information](https://go.dev/doc/devel/release#go1.26.5), [GO-2026-5856](https://pkg.go.dev/vuln/GO-2026-5856)

5. **The bcrypt-oriented 10-request/minute limiter wraps the entire player account UI.** Account pages, friends, remote-address management, 2FA, logout, and GET requests all consume the shared IP bucket. Normal users—and everyone behind the same NAT—can lock themselves out after ten page requests. Mount it only on password, signup, and verification endpoints. [players.go](internal/players/players.go#L103), [ip_middleware.go](internal/ratelimit/ip_middleware.go#L82)

6. **Realtime advertises four connections per player but the hub stores one.** Each new connection replaces the previous one. It also calls the old socket's potentially blocking close operation while holding the global hub mutex, stalling all tenants. Store a set of sockets per player and close removed sockets outside the lock. [hub.go](internal/realtime/hub.go#L53), [config.go](internal/config/config.go#L83)

7. **Conditional: tenant fleet configuration can select any Agones namespace.** If the service account has cluster-wide permissions, a tenant can create and delete GameServers outside its assigned namespace. Remove the override or enforce an operator-defined namespace mapping. Allocations with missing address or port can also be leaked. [fleets.go](internal/controlpanel/fleets.go#L378), [backend.go](internal/fleet/agones/backend.go#L167)

8. **There is no readiness endpoint, and River startup failures are non-fatal.** `/healthz` always returns OK while the database, cache, fleet, or background jobs may be unavailable. Failed job startup silently disables session, ticket, trusted-device, and storage-warning maintenance. Add `/readyz` and expose job health; decide which workers must block production startup. [health.go](internal/httpapi/health.go#L20), [main.go](cmd/ggscale-server/main.go#L511)

9. **`BootstrapQ` omits the configured statement timeout.** It is used broadly by control-panel and global-account requests. The Casbin adapter compounds this by using `context.Background()` for every operation, allowing authorization writes and reloads to outlive request deadlines indefinitely. [db.go](internal/db/db.go#L181), [adapter.go](internal/rbac/adapter.go#L24)

10. **Game-session fields have no useful size constraints.** Addresses, title IDs, props, and QoS can consume nearly the full 1 MiB request limit and are persisted into unconstrained `text` or `jsonb` columns. A 64-player roster can consequently generate very large buffered responses. Add field-specific caps and matching database constraints. [game_session.go](internal/httpapi/game_session.go#L26), [baseline migration](db/migrations/0001_baseline.up.sql#L883)

11. **The server browser is process-local.** In a multi-replica deployment, a heartbeat hitting one pod is invisible to list requests hitting another. New-entry admission also scans the entire global map under a write lock. Move it to the distributed cache or consistently route both operations. [registry.go](internal/serverlist/registry.go#L1), [registry.go](internal/serverlist/registry.go#L100)

12. **Storage size overrides above 1 MiB cannot work.** The application supports higher tenant or project limits, but the PUT operation does not override Huma's default 1 MiB body cap, so the framework rejects the request before the custom limit is resolved. [storage.go](internal/httpapi/storage.go#L70), [config.go](internal/config/config.go#L215)

## Low / nit

- Storage `key_prefix` is treated as a SQL `LIKE` pattern, so `%` and `_` are wildcards rather than literal prefix characters. [storage.sql](internal/db/queries/storage.sql#L128)
- `os.WriteFile(..., 0600)` does not repair permissions on an existing bootstrap-token file. Explicitly verify or chmod the file and write atomically. [bootstrap.go](internal/controlpanel/bootstrap.go#L87)
- Control-panel sessions extend their database expiry but do not refresh the browser cookie, so the advertised sliding session still ends at the original 12-hour cookie expiry. [auth.go](internal/controlpanel/auth.go#L113)
- Several TTL caches retain expired keys indefinitely unless those exact keys are reused or invalidated. Historical tenant and project cardinality therefore becomes permanent process memory. [cached.go](internal/storagelimit/cached.go#L62), [overrides.go](internal/ratelimit/overrides.go#L202), [rbac.go](internal/rbac/rbac.go#L258)
- Server-list keys concatenate unescaped user-controlled components with `:`, allowing same-tenant fleet/name collisions. Use a structured key or length-prefix encoding. [registry.go](internal/serverlist/registry.go#L94)

## Verification performed

- `golangci-lint run`: passed
- `go test -race ./...`: passed
- `make test-integration`: passed all integration packages
- `make e2e`: passed
- `go mod verify`: passed
- `git diff --check`: passed
- Online `govulncheck`: failed as described above

## Overall assessment

The overall Go style is solid: formatting is clean, errors are generally handled explicitly, the RLS design is thoughtful, and the test suite is substantial. The largest risks are concurrency and lifecycle edge cases and production architecture boundaries, not basic idiomatic-Go problems.
