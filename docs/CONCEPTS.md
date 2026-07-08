# ggscale concepts

A 5-minute orientation for first-time self-hosters. Read this before you boot the control panel and the rest of ggscale will make a lot more sense. The same content lives in-app at `/v1/control-panel/help` once you're signed in.

## The 30-second version

1. Run ggscale with `make up`. That brings up the server, Postgres, and a dev SMTP.
2. Bootstrap the control panel: paste the one-time token from `./data/bootstrap.token` at `http://localhost:3001/v1/control-panel/setup` and create your admin account.
3. Create a tenant. The "New tenant" page sets up a tenant, a starter project, and a first API key in one go. Copy the API key; it's only shown once.
4. Drop the API key into your game. Your game (or game server) sends `Authorization: Bearer <api_key>` to ggscale's HTTP API, and ggscale handles auth, storage, leaderboards, and friends behind that key.

## The pieces

Five terms show up across the control panel, the API, and the database. Tenants are the outer container; everything else lives inside one.

### Tenant

The isolation boundary. Usually one studio, customer, or game brand. Everything else (projects, API keys, player accounts, save data, leaderboards) belongs to exactly one tenant, and Postgres row-level security stops tenants from seeing each other's data.

You'll hit this on day one when you create your first tenant.

### Project

Projects partition a tenant's workloads. Most studios use one project per game (e.g. `arcade-prod`, `arcade-staging`). Splitting projects keeps prod and staging data apart while still rolling up to one tenant for billing and admin.

A tenant always has at least one starter project; you can add more from the control panel's **Projects** page.

### API key

How your game authenticates to ggscale. Always scoped to a tenant, and optionally pinned to a single project (otherwise it can act on any project in the tenant).

The plaintext value is generated once and only stored as a hash, so copy it when you create it. There's no way to recover it later. If one leaks, revoke it and create a new one.

### Control panel user

You. The humans who log in to manage tenants, projects, and keys. Control panel users are separate from players; they get tenant memberships with an `owner`, `admin`, or `member` role. API-key management needs `admin` or higher.

### Player

The people who actually play your game. They sign up, log in, and store data through ggscale's `/v1/auth/...` and storage APIs, which your game calls on their behalf using the tenant's API key. Players never touch this control panel.

### Player account (global)

A platform-wide gg-scale account (`player_accounts`) that sits *above* the per-project players. The same human is one account across every game; friends, remote addresses, invites, and tenant bans hang off it. A player with no linked account is an anonymous player. Signing up for a global account is always open; *linking* an account into a specific project is what the public-join toggles and invites gate.

### Suspension levels

Three independent kill switches, from narrowest to broadest:

| Level | Column / table | Scope | Blocks |
|---|---|---|---|
| **Project disable** | `project_players.disabled_at` | one player in one project | that player's login, session verify, gameplay in that project |
| **Tenant ban** | `tenant_player_bans` | one global account across every project a tenant owns | login/refresh/custom-token, session verify, project join/link, invite acceptance, matchmaker tickets, relay credentials — for all of that account's players in the tenant |
| **Platform disable** | `player_accounts.disabled_at` | the global account everywhere | all account-level auth (player site sign-in, account session) |

Project disable and tenant ban both bump `project_players.session_epoch`, and that epoch is embedded in the player's JWT (`sepoch` claim). Every player-authed request re-reads the player's current epoch and rejects a token whose epoch snapshot is stale — so does server-side session verification (`POST /v1/server/player-sessions/verify`). A disable or ban therefore takes effect **immediately** on the player's next request to any player route, rather than waiting out the 15-minute access-token TTL.

## How a request flows

```
your game ──► ggscale HTTP API
              │
              ├─ Authorization: Bearer <api_key>   → resolves to a Tenant (and maybe a Project)
              └─ X-Session-Token: <player_token>   → resolves to a Player inside that Tenant
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
open http://localhost:3001/v1/control-panel/setup

# 4. In the control panel, click "+ New tenant"
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
- In-app concepts page at `/v1/control-panel/help` once you're signed in.
