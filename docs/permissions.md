# Database roles & permissions

How ggscale authenticates to PostgreSQL and enforces least privilege at
runtime. This is the canonical reference; the role bundle itself is defined in
`db/migrations/0001_baseline.up.sql` and the enforcement lives in
`cmd/ggscale-server/main.go` + `internal/db/db.go`.

## Principle

The credential that is most exposed — the DSN in the app's environment, sent
over the tailnet, and present on the read replica — must be the **least
privileged**. Elevated rights (DDL, role creation, RLS setup) are needed only
for the brief migration step at startup, so they live behind a separate DSN.

Every application query executes as the non-superuser role **`ggscale_app`**,
which is subject to row-level security. Nothing the app does at runtime runs as
a superuser or a table owner.

## Roles

| Role | Login? | Purpose | Privileges |
|---|---|---|---|
| `ggscale_app` | No (`NOLOGIN`) | Runtime privilege bundle | DML on every app + `river_*` table, `USAGE,SELECT` on sequences, `EXECUTE` on helper functions. Non-superuser, non-owner, **not** `BYPASSRLS`. Created (guarded) and granted entirely by migration `0001` — 82 grants. |
| `ggscale_app_login` | Yes | Connection identity for the app pools | **None directly.** It is a member of `ggscale_app` and borrows those privileges only after `SET ROLE`. `NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION`. |
| owner / admin (e.g. `postgres`, or a dedicated `ggscale_owner`) | Yes | Applies migrations | DDL, `CREATE ROLE`, `FORCE RLS`, `CREATE POLICY`, `GRANT`. Owns the schema objects. Used only at startup, only via `DB_MIGRATE_URL`. |

`ggscale_app` deliberately stays `NOLOGIN`: it is a pure privilege bundle, never
a connection identity. The login role is a thin identity that `SET ROLE`s into
it. This matches the intent documented at the top of the baseline migration.

## Connection strings → roles

| Env var | Used by | Should authenticate as |
|---|---|---|
| `DATABASE_URL` | Primary + River pools (runtime) | `ggscale_app_login` |
| `DB_READ_URL` | Optional read-replica pool (east in production; empty aliases primary) | `ggscale_app_login` (on the replica) |
| `DB_MIGRATE_URL` | Migration runner at startup; required in production, empty falls back to `DATABASE_URL` elsewhere | owner / admin |

Both app hosts run migrations against the **west primary** at boot (golang-migrate
takes a lock, so concurrent boots are safe), then open their pools. Migrations
run only on the primary; DDL reaches the replica via physical replication.

## How enforcement works

1. **`SET ROLE` on every connection.** `newDBPool` (`cmd/ggscale-server/main.go`)
   installs an `AfterConnect` hook that runs `SET ROLE ggscale_app`. Whether the
   login role is `ggscale_app_login` (prod) or a superuser (zero-config
   self-host), the effective role for all subsequent queries is `ggscale_app`.
2. **Boot-time assertion.** `assertAppDBRole` runs against **both** the primary
   and read pools and refuses to start unless `current_user == ggscale_app` and
   that role does **not** own the tenant tables. In production it also checks
   `session_user` and rejects a login that owns, or can assume a role that owns,
   the tenant tables. `SET ROLE` cannot disguise that underlying identity, so a
   misconfigured credential fails the boot instead of recovering owner rights
   through `RESET ROLE` (Dokku keeps the old container serving).
3. **Row-level security.** Tenant tables have `ENABLE` + `FORCE ROW LEVEL
   SECURITY` with isolation policies keyed on
   `current_setting('app.tenant_id')`. `db.Pool.Q` opens a transaction, sets
   that GUC via `set_config(..., is_local => true)`, and applies
   `SET LOCAL statement_timeout` before running the caller's closure
   (`internal/db/db.go`). `BootstrapQ` is the tenant-less variant for
   pre-tenant / worker paths. Because `ggscale_app` is neither superuser nor
   owner, `FORCE RLS` applies to it with no exceptions.
4. **Read replica is read-only by construction.** The read pool is built with
   `NewReadPoolWithTimeout`, so its `Q` opens `pgx.TxOptions{AccessMode:
   ReadOnly}` transactions. A write accidentally routed there is rejected by
   PostgreSQL (SQLSTATE `25006`), verified in `tests/integration/db`. Only
   explicitly staleness-tolerant reads use `d.ReadPool` (see
   `docs/prod-upgrade.md` M2).

## River, LISTEN/NOTIFY, migrations

- **River** uses the raw primary pool, so its jobs also run as `ggscale_app`
  after `SET ROLE`. `ggscale_app` already holds DML grants on `river_client`,
  `river_client_queue`, `river_job`, `river_leader`, `river_queue`, and
  `river_job_id_seq`. Advisory locks and `LISTEN`/`NOTIFY` require no grant.
- **LISTEN/NOTIFY** (matchmaker ticket channel) runs on the primary only —
  `NOTIFY` fires on the node where the write commits and does not propagate to
  replicas.
- **Migrations** are the only operation needing more than `ggscale_app`, which
  is why they use `DB_MIGRATE_URL`.

## Setting up the login role (bw-ops)

`ggscale_app` already exists (migration `0001`). Add only the login identity;
roles are cluster-global and replicate to the standby automatically.

```sql
-- 1. Least-privilege LOGIN identity (password injected from Vault):
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ggscale_app_login') THEN
    CREATE ROLE ggscale_app_login LOGIN PASSWORD :'app_login_pw'
      NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;
  END IF;
END $$;

-- 2. Can become ggscale_app, but holds no ambient privilege until it does
--    (INHERIT FALSE = a connection that skips SET ROLE can touch nothing).
--    PG16+ syntax; production is PG17.
GRANT ggscale_app TO ggscale_app_login WITH INHERIT FALSE, SET TRUE;

-- 3. Explicit connect (covers a revoked-PUBLIC setup):
GRANT CONNECT ON DATABASE ggscale_pg TO ggscale_app_login;
```

Verify:

```sh
# Effective role is ggscale_app after SET ROLE:
psql "postgres://ggscale_app_login:***@<host>:5432/ggscale_pg?sslmode=disable" \
  -c "SET ROLE ggscale_app; SELECT current_user;"     # -> ggscale_app

# Without SET ROLE it can touch nothing (INHERIT FALSE):
psql "postgres://ggscale_app_login:***@<host>:5432/ggscale_pg?sslmode=disable" \
  -c "SELECT count(*) FROM tenants;"                   # -> permission denied
```

## Production rollout and current routing

The least-privilege login rollout completed on 2026-07-16. Later that day,
`database-west-1` was promoted to the write primary and
`database-east-1` was rebuilt as its streaming read replica.

| App host | `DATABASE_URL` | `DB_MIGRATE_URL` | `DB_READ_URL` |
|---|---|---|---|
| `dokku-west-1` | `ggscale_app_login` on west primary | admin on west primary | empty; aliases the west primary |
| `dokku-east-1` | `ggscale_app_login` on west primary | admin on west primary | `ggscale_app_login` on east replica |

Only explicitly staleness-tolerant east requests use the east replica.
Writes, migrations, River, LISTEN/NOTIFY, and read-after-write paths use the
west primary. The boot-time `assertAppDBRole` check gates every configured
pool.

**Credential rollback:** restore the prior `ggscale_app_login` credential or
roll back the application release while repairing it. Current production
releases reject an admin DSN in `DATABASE_URL`; do not point writes at a
replica.

## Self-hosting / zero-config

None of this is required to run ggscale. Leave `DB_MIGRATE_URL` unset and point
`DATABASE_URL` at any role that can both apply migrations and connect (commonly
the local superuser). The `AfterConnect` `SET ROLE ggscale_app` and the
`assertAppDBRole` boot check still force every runtime query through the
non-superuser, RLS-subject `ggscale_app` role, so tenant isolation holds
regardless of the login role. The dedicated login role is a production
hardening, not a functional requirement.

## Cross-references

- Role bundle + RLS policies + grants: `db/migrations/0001_baseline.up.sql`
- Pool construction, `SET ROLE`, boot assertion: `cmd/ggscale-server/main.go`
  (`newDBPool`, `assertAppDBRole`)
- Tenant GUC / transaction helper / read-only pool: `internal/db/db.go`
- Read/write pool split and routing: `docs/prod-upgrade.md` (M2)
