# Prod upgrade plan: write primary, read replicas, second app host per region

Implementation plan, 2026-07-15. Sequences the infra/service upgrades that
make the tier_1/tier_2 ladder numbers (`docs/pricing-strategy.md`) honest as
managed capacity. Grounds: `docs/capacity-and-launch.md` (current topology,
tripwires), `docs/temp/connection-capacity.md` (budget + code tasks), and
`docs/in-memory-cache-rollout.md` (process-local cache plus PostgreSQL regional
capacity grants). Infra execution lives in bw-ops; this doc is the cross-repo
checklist.

Current topology (2026-07-16 switchover): `dokku-east-1` +
`dokku-west-1` (b3-8, app-only) behind a Cloudflare geo LB; Postgres 17
write primary on `database-west-1` and streaming hot standby on
`database-east-1` (both b3-16 with 250 GB volumes, tailnet-only);
monitoring on `data-east-1`. ~$354/mo.

## Milestones at a glance

| # | What | Trigger | ~Cost delta |
|---|---|---|--:|
| M1 | Write-primary hardening | completed 2026-07-16 | $0 |
| M2 | Regional replica + read-split | completed 2026-07-16 | +$112/mo |
| M3 | Second app host per region | memory/grant rollout gates pass + app CPU >60% warn, or first tier_2 promise | +$86/mo |
| M2b | Promote west; rebuild east as replica | completed 2026-07-16 | $0 |

Each milestone is self-funding at the tier prices under discussion: one
tier_1 tenant covers M2; one tier_2 covers M2+M3 combined.

## M1 — Write primary hardening (~$0)

Server settings (bw-ops, `postgres_primary` role so a rebuild keeps them):

- [x] `max_connections = 200` (stock 100 leaves no room for M2 WAL senders or
      M3's two extra app pools — budget table below).
- [x] `shared_buffers = 4GB` (stock 128 MB wastes the 16 GB box).
- [x] Document both in bw-ops next to the connection budget.

Alerting (bw-ops):

- [x] Add the 70% **warn** tier to the existing 85% connections page, so
      budget violations surface with runway.
- [x] Per-instance app pool-saturation alerts
      (`connection-capacity.md` task 4): `empty_acquire_total` rate > 0
      warn; acquired/max > 0.8 warn, > 0.95 page. With west now primary, east
      is expected to feel cross-region write pressure first.

Backups (bw-ops) — the original same-volume nightly dump has been replaced:

- [x] Move backups off-volume — WAL-G ships base backups + archived WAL to a
      Cloudflare R2 bucket (nightly 03:00 UTC, 7 full retained), off the data
      volume. libsodium client-side encryption (`ggscale-pg/encrypted-v1`
      prefix; key held only in Ansible Vault).
- [x] Adopt base-backup + WAL archiving (WAL-G, zstd) in place of nightly full
      dumps: bounded RPO, PITR, and the restore path a paying tier_2 customer
      assumes exists. Also the prerequisite for cheap replica bootstrap in M2.
- [x] Restore drill: full keyed restore run in isolation on database-east-1
      (prod PGDATA untouched, archiving off) — system identifier, migration
      state, `ggscale_app` role, and row counts all matched prod; recorded in
      bw-ops `database-dokku.md`.

Service code (this repo, TDD — details in `connection-capacity.md` tasks
2–3, tracked here for sequencing):

- [x] Pool idle trim: `DB_MAX_CONN_IDLE_TIME` (default 10m) wired to
      `poolCfg.MaxConnIdleTime`.
- [x] Request-deadline middleware: `HTTP_REQUEST_TIMEOUT` (default 15 s,
      below the 30 s `WriteTimeout`), 503 + Retry-After on expiry;
      WebSocket/streaming paths exempt. Turns pool-exhaustion brownouts into
      crisp errors.
- [~] Load-test scenario measuring CCU → queries/sec: scenario authored in
      `perf-ggscale/docs/capacity-scenario.md` (`connection-capacity.md` task 6);
      the live measurement run against east/west is still pending. The one
      unmeasured input every capacity claim below depends on.

Escalation levers (no action until tripwires fire): b3-16 → b3-32
(~+$64/mo) on sustained DB CPU >60%; volume grow (online, +IOPS at
30 IOPS/GB) at 80% usage.

## M2 — Read replicas

### Outcome: west primary, east replica

The first M2 deployment made west the replica because east was then the
primary. On 2026-07-16, with accepted downtime, operations quiesced both app
hosts, verified zero observed replay lag, fenced east, promoted west, and
rebuilt east from west's new timeline. The current topology keeps writes close
to the west region while retaining east as a regional read replica and manual
promotion target.

- Both app hosts' `DATABASE_URL` and `DB_MIGRATE_URL` target the west
  primary with the runtime and migration identities respectively.
- `dokku-west-1` leaves `DB_READ_URL` empty and reads locally from west.
- `dokku-east-1` sends explicitly staleness-tolerant reads to the east
  replica; writes and read-after-write paths still cross the tailnet to west.
- Both database hosts are currently the same OVH `b3-16` size. The change
  improves geography for west traffic, not database instance capacity.

### Infra (bw-ops)

- [x] Provision `database-west-1` (OVH US-WEST-OR-1, b3-16 + 250 GB volume,
      Tailscale `100.110.187.0`, tailnet-only, public 5432 filtered; same
      hardening as database-east-1).
- [x] Streaming replication from database-east-1 (bootstrapped from the
      encrypted WAL-G/R2 LATEST base backup; `primary_conninfo` over the
      tailnet; `ggscale_replication` login + slot `database_west_1`;
      `hot_standby=on`; verified receiver streaming, lag 0). This records
      the original M2 direction before the 2026-07-16 switchover.
- [x] Planned switchover: promote `database-west-1`, take a new encrypted
      timeline-2 base backup, replace east's old primary data, and rebuild
      `database-east-1` as a streaming standby using slot
      `database_east_1`. Verified read-only with lag 0.
- [x] Monitoring: replication-lag gauge + alert (warn > 30 s, page > 5 m),
      replica pg_up, volume/CPU panels — Alloy pattern; lag host-selection
      follows `database_replica_hosts` so it survives a role reversal.
- [x] Failover runbook: `docs/postgres-replica-promotion-guide.md` (bw-ops);
      drilled on scratch VMs (promote 8.9 s, repoint 21 s, old-master rebuild
      48 s), then exercised in production during the west-primary switchover.
      The runbook is topology-driven: promote the designated replica, repoint
      both app hosts' `DATABASE_URL`, and rebuild the old primary as a
      replica.

### Service code (this repo, TDD)

Routing mechanism (decided): **by injection, not detection.** Two pools; when
`DB_READ_URL` is empty the read pool aliases the primary pool, so all hosts
run identical code and only the app host colocated with the current replica
sets the env var (currently east). Two sqlc `Queries` handles (`primaryQ`,
`readQ`); stores serving staleness-tolerant reads are constructed with
`readQ`, everything else keeps `primaryQ`. No SQL inspection, no automatic
SELECT routing — only the call site knows when a read must observe a write it
just made (session validation after login, matchmaking state, invite
acceptance), so the replica list is an explicit, grep-able set of constructor
decisions with primary as the default.

- [x] Optional read-pool config: `DB_READ_URL` (empty = feature off, read
      pool aliases the primary — zero-config self-host unchanged), plus
      `DB_READ_MAX_CONNS` (default 25). Same `SET ROLE` AfterConnect,
      statement timeout, and `pool="read"` pool-gauge label (query-duration
      histogram shared across pools by design).
- [x] Route explicitly chosen read-only, staleness-tolerant queries to the
      read pool — the hot paths: leaderboard top/around-me, friends list,
      presence read fan-out, storage GET/list. Writes, auth/session issuance,
      matchmaking state, and anything read-after-write stays on the primary.
      Opt-in per query site (`d.ReadPool.Q`), never automatic.
- [x] River, LISTEN/NOTIFY, and migrations always use the primary pool.
      Enforced by construction: the read pool opens read-only transactions
      (`pgx.ReadOnly`), so a write routed there is rejected by Postgres
      (verified in `tests/integration/db`, SQLSTATE 25006).
- [x] Deploy after switchover: `DB_READ_URL` set on **dokku-east-1** only
      (points at its local replica); west leaves it empty and aliases reads to
      its local primary. Live in prod 2026-07-16 — `pg_up=1` on both hosts,
      east reports `pg_replication_is_replica=1`, and lag is 0.

## M3 — Second app host per region

Prerequisite: migration `0014_realtime_connection_grants` is deployed,
`APP_REGION` is correct on every host, and the regional grant tests in
`docs/IN_MEMORY_CACHE_AND_REGRESSION_TEST_PLAN.md` pass. All four apps use
process-local caches and coordinate only the tenant CCU envelope through the
write-primary PostgreSQL endpoint.

For the initial rollout, set `APP_REGION` through bw-ops first. The subsequent
one-host application deploy runs migration 0014 with `DB_MIGRATE_URL` before
the server starts; do not deploy first and try to repair the region value
afterward.

- [ ] Provision `dokku-east-2` + `dokku-west-2` (b3-8, ~$43/mo each),
      tailnet join, Dokku app deploy mirroring host 1 (document the 4-host
      deploy order in bw-ops: one host at a time, region at a time).
- [ ] Deploy one host at a time. Verify migration 0014, `APP_REGION`, grant
      renewal, and bounded emergency metrics before admitting each host to its
      Cloudflare origin pool. No cache ports or peer discovery are required.
- [ ] Cloudflare LB: add both new origins to their region pools; confirm
      health checks and geo steering.
- [ ] Monitoring: scrape the new hosts (Alloy pattern), add them to the CPU
      tripwire alerts; app-pool saturation alerts are per-instance already.
      Alert on grant-sync errors and any emergency admissions.
- [ ] Failure drills: stop one app without graceful shutdown and verify its
      reserved capacity is reusable within 45 seconds; block its primary DB
      path and verify admissions stop at the bounded local emergency allowance;
      verify east and west capacity envelopes remain independent. Confirm the
      hourly River sweep removes the stopped holder's expired row.
- [ ] Recompute and commit the connection budget (bw-ops), now:

| Client of database-west-1 | Connections |
|---|--:|
| 4 × app `DB_MAX_CONNS` (east-1/2, west-1/2) | 100 |
| WAL sender → database-east-1 (M2) | 2 |
| Alloy postgres exporter | 2 |
| WAL-G backup / administration | 2 |
| admin / migrations headroom | 3 |
| **Total peak** | **~109** |

  Fits `max_connections = 200` (M1) with headroom for an incident-mode pool
  raise; does **not** fit the stock 100 — M1 is a hard prerequisite.

- [ ] Re-run the load-test scenario (M1) against the 4-host topology; record
      the measured CCU → qps ratio in `connection-capacity.md`.

## Sequencing

1. **M1 write-primary hardening** — completed 2026-07-16.
2. **Process-local cache + PostgreSQL grant implementation** (code,
   independent of new hardware) in parallel with M1.
3. **M2 regional replica/read split and west-primary switchover** — completed
   2026-07-16.
4. **M3** as a fast-follow once the grant rollout gates pass and either app CPU
   trips 60% or a tier_2 conversation starts.
5. b3-32 / volume grow: on their written triggers only.

## Cross-references

- Tier ladder + what each class promises: `docs/pricing-strategy.md`
- Capacity model + tripwires: `docs/capacity-and-launch.md`
- Connection budget + pool/deadline code tasks: `docs/temp/connection-capacity.md`
- Cache and regional grant design (M3 prerequisite):
  `docs/in-memory-cache-rollout.md`
- Infra source of truth / runbooks: bw-ops (`database-dokku.md`)
