# ggscale concepts

A 5-minute orientation for first-time self-hosters. Read this before you boot the dashboard and the rest of ggscale will make a lot more sense. The same content lives in-app at `/v1/dashboard/help` once you're signed in.

## The 30-second version

1. Run ggscale with `make up`. That brings up the server, Postgres, and a dev SMTP.
2. Bootstrap the dashboard: paste the one-time token from `./data/bootstrap.token` at `http://localhost:3001/v1/dashboard/setup` and create your admin account.
3. Create a tenant. The "New tenant" page sets up a tenant, a starter project, and a first API key in one go. Copy the API key; it's only shown once.
4. Drop the API key into your game. Your game (or game server) sends `Authorization: Bearer <api_key>` to ggscale's HTTP API, and ggscale handles auth, storage, leaderboards, and friends behind that key.

## The pieces

Five terms show up across the dashboard, the API, and the database. Tenants are the outer container; everything else lives inside one.

### Tenant

The isolation boundary. Usually one studio, customer, or game brand. Everything else (projects, API keys, player accounts, save data, leaderboards) belongs to exactly one tenant, and Postgres row-level security stops tenants from seeing each other's data.

You'll hit this on day one when you create your first tenant.

### Project

Projects partition a tenant's workloads. Most studios use one project per game (e.g. `arcade-prod`, `arcade-staging`). Splitting projects keeps prod and staging data apart while still rolling up to one tenant for billing and admin.

A tenant always has at least one starter project; you can add more from the dashboard's **Projects** page.

### API key

How your game authenticates to ggscale. Always scoped to a tenant, and optionally pinned to a single project (otherwise it can act on any project in the tenant).

The plaintext value is generated once and only stored as a hash, so copy it when you create it. There's no way to recover it later. If one leaks, revoke it and create a new one.

### Dashboard user

You. The humans who log in to manage tenants, projects, and keys. Dashboard users are separate from end users; they get tenant memberships with an `owner`, `admin`, or `member` role. API-key management needs `admin` or higher.

### End user (player)

The people who actually play your game. They sign up, log in, and store data through ggscale's `/v1/auth/...` and storage APIs, which your game calls on their behalf using the tenant's API key. End users never touch this dashboard.

## How a request flows

```
your game ──► ggscale HTTP API
              │
              ├─ Authorization: Bearer <api_key>   → resolves to a Tenant (and maybe a Project)
              └─ X-Session-Token: <player_token>   → resolves to an End user inside that Tenant
              │
              ▼
       Postgres (Row-Level Security)
       Only rows belonging to that tenant_id are visible.
```

The API key says *which game* is calling. The player session, when present, says *which player*. The database refuses to return anything from a different tenant.

A stolen player session won't work under a different tenant's API key, because the auth layer checks that the session's tenant matches the key's tenant. See `docs/ARCHITECTURE.md` § "Auth headers" for details.

## End-to-end walkthrough

```bash
# 1. Bring up the stack
make up

# 2. Read the one-time bootstrap token
cat ./data/bootstrap.token

# 3. Paste it at the setup page and create your admin account
open http://localhost:3001/v1/dashboard/setup

# 4. In the dashboard, click "+ New tenant"
#      Tenant name:     my-studio
#      Starter project: arcade-prod
#      Copy the API key shown once.

# 5. Make your first authenticated call
curl -H "Authorization: Bearer INSERT_API_KEY_HERE" \
     http://localhost:8080/v1/storage/objects
```

You should get back an empty object list. That's a working tenant. From here, point your game's SDK at `http://localhost:8080` with the same bearer key.

## See also

- [`docs/ARCHITECTURE.md`](ARCHITECTURE.md) — RLS layout, tenant middleware, auth headers (`Authorization` vs `X-Session-Token`), audit log.
- [`docs/SELF_HOSTING.md`](SELF_HOSTING.md) — production setup, game server tiers, UDP security.
- In-app concepts page at `/v1/dashboard/help` once you're signed in.
