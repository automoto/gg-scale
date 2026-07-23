# Capacity & launch plan

Decision record, 2026-07-11; **updated 2026-07-16 after the west-primary
switchover** (bw-ops `database-dokku.md`). Grounds the tier limits in
`docs/pricing-strategy.md` against the real production infra in `bw-ops`,
and lists the upgrades to make before and after a marketing-driven launch this
month.

## Current production infra (summary)

- **Two app hosts**, OVH b3-8, one per region: `dokku-east-1` (US-EAST-VA-1)
  and `dokku-west-1` (US-WEST-OR-1), active/active behind a Cloudflare
  geo-steered load balancer. **Both are app-only** since the DB cutover.
- **Postgres 17 write primary** on dedicated `database-west-1`
  (US-WEST-OR-1) with `database-east-1` (US-EAST-VA-1) as its streaming hot
  standby. Both are b3-16 (4 vCPU / 16 GB), tailnet-only, with attached
  **250 GB high-speed-gen2 volumes** (~7,500 IOPS, grows online).
  `max_connections=200` and `shared_buffers=4GB`.
- Both app hosts reach PostgreSQL **over Tailscale**. West writes and reads
  locally. East writes and read-after-write operations cross the
  ~60–70 ms path to west; explicitly staleness-tolerant east reads use the
  local east replica.
- Encrypted WAL-G base backups and continuous WAL archiving are stored
  off-host in Cloudflare R2. The nightly base-backup job runs only on the
  current primary.
- **Monitoring** on `data-east-1` (Prometheus/Grafana/Alertmanager/Loki,
  tailnet-only). Postgres internals covered since 2026-07-13 via Alloy's
  `prometheus.exporter.postgres`: connections-vs-max, TPS, cache hit, volume
  usage dashboard + `infra-postgres` alert rules.
- Database role, replication, backup, and read-routing monitoring follows the
  bw-ops primary/replica inventory.
- Cost ~$354/mo (3 × b3-8 ≈ $130 + two b3-16 database hosts ≈ $224).

## The binding constraint

The pre-cutover constraint — app and Postgres fighting over one shared box
and one 50 GB disk — is **gone**. The remaining ceilings, in the order a
marketing spike would hit them:

1. **App-host CPU per region** (b3-8): request handling, WebSocket hubs,
   matchmaker. Scale is a flavor resize.
2. **East's RTT-bound primary pool**: writes, auth, matchmaking state, and
   read-after-write queries cross the country to west. Explicitly
   staleness-tolerant reads use east's local replica.
3. **database-west-1 CPU/IOPS** (4 vCPU): isolated from app load;
   resize b3-16 → b3-32 (~$176/mo) when metrics demand.

Self-hosters run their own infra and are unaffected — this governs the managed
side only.

## What the infra holds (back-of-napkin)

**CCU is the wrong unit.** An idle connected player costs the infra almost
nothing: the WebSocket is tens of KB in the Go hub, and the 30 s heartbeat
refreshes a slot in the cache (`internal/realtime/server.go`), never
Postgres. Connection *count* is effectively free into the tens of thousands
per app host. What costs is **DB-touching actions/sec** (login, matchmaking
ticket, save, leaderboard write, session create/end):

    DB load ≈ CCU × actions-per-player-per-sec
    busy DB connections ≈ that qps × avg query seconds

The per-player action rate is game-dependent and **unmeasured** — pinning it
is `docs/temp/connection-capacity.md` task 6. Illustrative envelope: at one
DB action per player per 10 s (chatty) and few-ms queries, 20k CCU ≈ 2,000
qps ≈ single-digit busy connections — comfortable on the dedicated NVMe
Postgres. On those assumptions the DB box doesn't press until ~50k+ CCU, and
app-host CPU (resize or add hosts behind CF) binds first. Treat any CCU
number here as a checkpoint to compare dashboards against, not a wall.

| Resource | Steady-state cost | Real ceiling | Lever |
|---|---|---|---|
| Connected players (CCU) | RAM + cache refreshes, no DB | app-host CPU from action throughput | resize/add app hosts |
| DB actions/sec | busy conns = qps × query time | database-west-1 CPU (4 vCPU), then IOPS | b3-32 resize; volume grow (IOPS scale with size) |
| DB connections | ~56 budgeted | 85% of `max_connections` (alerted) | budget + settings in `docs/temp/connection-capacity.md` |
| Registered players / saves (JSONB) | volume bytes, not rows | 80% of the 250 GB volume (alerted) | grow volume online |

- **Most likely first failure:** east's primary pool saturation under
  write-heavy load (RTT-inflated slot hold time), or an app-host CPU limit.
  Both are alerted;
  plan and runbook: `docs/temp/connection-capacity.md`.
- **Highest-impact failure:** `database-west-1` down interrupts writes and
  primary-consistent reads in both regions. East is a promotion target, but
  promotion is manual and requires accepted downtime and strong fencing.
  Encrypted off-host WAL-G backups provide a separate recovery path.

## Verdict for launch

The dedicated database hosts, regional read split, streaming replica, and
off-host backups remove the worst pre-cutover risks. Current infra survives a
**soft/modest launch** with comfortable margin. A primary outage still causes
write downtime because promotion is operator-driven, but it no longer requires
a restore from backup before service can resume.

## Upgrades

Execution plan for the items below (write-primary hardening, regional
replication/read split, and a second app host per region):
`docs/prod-upgrade.md`.

### Done (2026-07-13 cutover, bw-ops `database-dokku.md`)

- [x] Postgres on its own box: `database-east-1` (b3-16 + 250 GB volume,
      ~$112/mo). Removes app↔DB CPU contention and the shared-50-GB-disk
      risk; unlocks the higher CCU tiers.
- [x] Backups initially moved off the app host to database-east-1's volume.
      This was superseded on 2026-07-16 by encrypted off-host WAL-G/R2 base
      backups and continuous archiving.
- [x] Postgres-internal monitoring: Alloy `prometheus.exporter.postgres` +
      Grafana postgres dashboard + `infra-postgres` alert rules (pg_up,
      volume >80%, connections >85% of max).

### Done (2026-07-16 hardening and regional database work)

- [x] Execute the connection-capacity foundation
      (`docs/temp/connection-capacity.md`): explicit connection budget,
      `max_connections=200` + `shared_buffers=4GB`, pool idle trim, request
      deadlines, and per-instance pool-saturation alerts. The live CCU→load
      measurement remains pending.
- [x] Provision the second database host, add encrypted WAL-G/R2 backup and
      continuous archiving, establish streaming replication, and ship the
      explicit read-pool split.
- [x] Promote `database-west-1` as write primary and rebuild
      `database-east-1` as its read-only streaming replica. Route east's
      staleness-tolerant reads locally.

### Do now (pre-marketing, ~$0)

- [ ] Apply the staged `river_job` reindex GRANT fix.
- [ ] Ship relay + dedicated-fleet grants OFF (BYO only) — no managed relay/
      fleet infra exists.

### Later (real scale)

- [ ] Resize app boxes when host CPU sustains >60%; database-west-1
      b3-16 → b3-32 when DB CPU/IOPS demand it.
- [ ] Automated PostgreSQL failover, EU region, managed-fleet return,
      dedicated relay VM. Manual fenced promotion is documented and tested.

## Tripwires (Alertmanager)

The point: marketing-driven growth warns you with days of runway before an
outage. If growth is quiet, nothing fires and nothing is spent.

| Metric | Warn | Page | Action forced |
|---|--:|--:|---|
| Aggregate CCU (sustained 10m) | 5,000 | 10,000 | Growth checkpoint, not a wall: compare CPU/pool/DB dashboards against headroom, resize whichever is trending |
| Postgres connections (% of max) | 70% (shipped) | 85% (shipped) | Runbook in `docs/temp/connection-capacity.md` |
| App pool saturation (per instance, shipped) | acquired >80% / empty-acquires >0 | acquired >95% | Same runbook — expect east first with west as primary |
| Database volume usage, either host | — | 80% (shipped) | Grow volume online (`ovhcloud` resize + `resize2fs`) |
| Host CPU, any host (sustained) | 60% | 80% | Resize flavor (app b3-8 up; DB b3-16 → b3-32) |

Volume grow and flavor resizes are short `make`/CLI operations, so the warn
tiers give ample runway before the pages.

## Cross-references

- Tier model and enforcement: `docs/pricing-strategy.md` (ladder amended
  2026-07-14; implementation plan `docs/temp/tier-rework.md` — note the new
  per-tenant CCU guardrails sit above the aggregate tripwires below by
  design: tripwires are growth checkpoints, tier caps are abuse ceilings)
- Competitor/support pricing research: `docs/archive/pricing-strategy.md`
- Infra source of truth: `bw-ops` — current DB topology in
  `database-dokku.md`; promotion procedure and production execution record
  in `docs/postgres-replica-promotion-guide.md` and
  `docs/postgres-west-primary-switchover-plan.md`.
- Connection budget, pool fixes, saturation runbook:
  `docs/temp/connection-capacity.md`
