# Project management API and MCP

ggscale has a project management API for one Game Project. Two types of
client can use it:

- A coding agent such as Claude Code, Cursor, or Codex, through the MCP
  (Model Context Protocol) endpoint `POST /mcp`.
- A script or a CLI, through plain HTTP requests to `/v1/project/...`.

Both use a management token (`ggm_...`). An agent can use it to set up a Game
Project and to find why online play fails.

Each group of operations needs a scope: a read scope for the reads of a
resource, and a write scope for its writes. A token made before read scopes
existed has no read access. Make a new token.

## For game developers

### 1. Make a token

1. In the control panel, open the Game Project, then **Game Project settings**.
2. In the **Management tokens** card, select **Manage tokens**. The page is
   `/v1/control-panel/tenants/{tenantID}/projects/{projectID}/management-tokens`.
3. Enter a label and the number of days until the token expires (default 30).
4. In the access table, select **No access**, **Read**, or **Read and write**
   for each resource. The default is **Read** in each row, so a new token is
   read-only. **Quick fill** sets all rows to **Read only** or to **Read and
   write**. An option that you cannot give is disabled, with the reason. Then
   select **Create token**.
5. Copy the token (`ggm_...`). It is shown one time only.

The **Management tokens** card shows only when the operator has turned the
feature on. Only an `owner` or `admin` of the Account Tenant can make a token.

### 2. Add the server to your agent

Claude Code:

```sh
claude mcp add --transport http ggscale https://<host>/mcp \
  --header "Authorization: Bearer ggm_..."
```

Other agents (Cursor, Codex, and others): add a streamable HTTP server with
the URL `https://<host>/mcp` and the header `Authorization: Bearer ggm_...`.

Then ask the agent, for example: "Use project_health_check and tell me why
matchmaking does not work."

The server supports the MCP protocol revisions `2026-07-28` (stateless),
`2025-11-25`, and `2025-06-18`. It has tools only (no resources and no
prompts). `GET /mcp` answers 405.

### Read scopes

| Scope | Tool | HTTP | What it does |
|---|---|---|---|
| none | `project_health_check` | `GET /v1/project/health` | Lists setup problems: API keys and their scopes, feature grants, server feature switches, relay, CORS origins, Steam sign-in, remote config size, quota state, and the number of live leaderboards against `LEADERBOARD_MAX_PER_PROJECT`. The API key check needs `keys:read`; the CORS, Steam, and remote config checks need `config:read`. Without the scope, the check is left out. |
| `matchmaking:read` | `matchmaking_ticket_trace` | `GET /v1/project/matchmaking/ticket-trace` | Shows one matchmaking ticket by `ticket_id`, or the queued ticket of a player by `player_id`. Finished tickets are kept for 24 hours. |
| `matchmaking:read` | `get_matchmaking_queue` | `GET /v1/project/matchmaking/queue` | The queue buckets of the dashboard queue page: ticket count, oldest ticket, and the min and max count of each bucket. No player data. Paged with `limit` and `cursor`. |
| `config:read` | `get_remote_config` | `GET /v1/project/remote-config` | The remote config and its revision number. |
| `config:read` | `get_steam_auth` | `GET /v1/project/steam-auth` | The Steam App ID (null if not set) and whether a Web API key is set. The key is never returned. |
| `leaderboards:read` | `list_leaderboards` | `GET /v1/project/leaderboards` | Leaderboard settings (no scores), at most 50. With `include_deleted`, also the deleted boards that you can restore. |
| `leaderboards:read` | `get_leaderboard` | `GET /v1/project/leaderboards/{leaderboard_id}` | The settings and the revision number of one leaderboard. |
| `keys:read` | `list_api_keys` | `GET /v1/project/api-keys` | Active keys for the Game Project, at most 50. Key values are never shown. |
| `players:read` | `list_players` | `GET /v1/project/players` | Players of the Game Project, newest first, paged. Player ID, external ID, status flags, and timestamps. Filters `player_id` and `external_id` (exact match). |
| `players:read` | `get_player` | `GET /v1/project/players/{player_id}` | One player. A player of another Game Project is 404. |
| `fleet:read` | `list_fleets` | `GET /v1/project/fleets` | Fleets (dedicated server templates), sorted by name, paged. Needs `FEATURE_FLEET_ENABLED` on the server and the `dedicated_servers` feature grant. Without the switch the fleet tools do not exist. |
| `fleet:read` | `get_fleet` | `GET /v1/project/fleets/{fleet_id}` | One fleet. Its settings are shown for the agones backend only, not for a plugin backend. A fleet of another Game Project is 404. |
| `fleet:read` | `list_allocations` | `GET /v1/project/allocations` | Game server allocations, newest first, paged. `include_terminal` adds shutdown and failed ones. Shows the server address, not labels or join data. |
| `fleet:read` | `get_allocation` | `GET /v1/project/allocations/{allocation_id}` | One allocation. An allocation of another Game Project is 404. |
| `fleet:read` | `list_allocation_events` | `GET /v1/project/allocations/{allocation_id}/events` | The last 50 status events of one allocation, newest first, paged. |
| `players:write` | `set_player_disabled` | `PUT /v1/project/players/{player_id}/disabled` | Body `{"disabled": true or false}`. Disables or enables the player in this Game Project. 409 while a delete request is pending. Returns the player. |
| `players:write` | `request_player_delete` | `POST /v1/project/players/{player_id}/delete-request` | Disables the player, ends its sessions, and starts the grace period (`PLAYER_DELETE_GRACE_PERIOD`). 409 when an administrator already requested it. Returns the player. |
| `players:write` | `cancel_player_delete` | `DELETE /v1/project/players/{player_id}/delete-request` | Cancels a pending deletion. 404 when none is pending. Returns the player. |
| `config:read` or `leaderboards:read` | `list_revisions` | `GET /v1/project/revisions` | The newest 3 revisions of the remote config (`resource=remote_config`, needs `config:read`) or of a leaderboard (`resource=leaderboard` and `leaderboard_id`, needs `leaderboards:read`). |

The player tools leave out `email` and `display_name` unless the token also
has the risky scope `players:pii` (it needs `players:read`). They are text
that players wrote: an agent must not follow instructions in them. The
player scopes need permission to manage players for the person who made the
token.

`fleet:read` needs permission to read fleets for the person who made the
token. The allocation tools also need permission to read allocations.

`matchmaking:read` also needs permission to read the matchmaker for the
person who made the token. The other read scopes need permission to manage
the Game Project, as the dashboard pages do.

### Write scopes

| Scope | Tool | HTTP | Undo |
|---|---|---|---|
| `config:write` | `set_remote_config` | `PUT /v1/project/remote-config` | `rollback_remote_config` |
| `config:write` | `rollback_remote_config` | `POST /v1/project/remote-config/rollback` | roll back again |
| `leaderboards:write` | `create_leaderboard` | `POST /v1/project/leaderboards` | `delete_leaderboard` |
| `leaderboards:write` | `update_leaderboard` | `PUT /v1/project/leaderboards/{leaderboard_id}` | `rollback_leaderboard` |
| `leaderboards:write` | `rollback_leaderboard` | `POST /v1/project/leaderboards/{leaderboard_id}/rollback` | roll back again |
| `leaderboards:write` | `delete_leaderboard` | `DELETE /v1/project/leaderboards/{leaderboard_id}` | `restore_leaderboard` |
| `leaderboards:write` | `restore_leaderboard` | `POST /v1/project/leaderboards/{leaderboard_id}/restore` | `delete_leaderboard` |
| `keys:create` | `create_api_key` (publishable `ggp_` keys only) | `POST /v1/project/api-keys` | `revoke_api_key`, or revoke the key in the dashboard |
| `origins:write` | `set_allowed_origins` | `PUT /v1/project/allowed-origins` | call it again with `old_origins` from its result |

- The dashboard offers only the scopes that you have permission for.
- A tool outside the scopes of the token is not in the agent's tool list. A
  call to it answers `unknown tool`. A direct HTTP request answers 403 with a
  message that names the scope.
- `set_remote_config`, `update_leaderboard`, and the rollback operations need
  `expected_revision`: the newest revision number from the read operation. A
  stale number is refused (HTTP 409), so the agent cannot overwrite a change
  that it did not see.
- `set_remote_config` takes `config`, and the leaderboard writes take
  `metadata`. Both are JSON objects, not strings. Large integers stay exact.
  A value that is not a JSON object is refused.
- `update_leaderboard` replaces all settings, as the dashboard form does. A
  setting that you do not send is removed. Read the board first.
- The newest 3 revisions of the remote config and of each leaderboard are
  kept. A leaderboard rollback restores settings, not scores.
- An argument that the operation does not have is refused. In MCP, the answer
  is a JSON-RPC invalid-params error (`-32602`) that names the argument. In
  HTTP, an unknown body property answers 422.
- `create_api_key` gives the key the label
  `<label> (management token: <token label>)`. The key value is in the result
  one time only.
- No operation can delete a Game Project. A player delete has a grace
  period and can be cancelled.
- The audit log shows each write as `management_token:<id>`, with the person
  who made the token. `list_revisions` shows the token ID as
  `management_token_id` and the source as `token`.

### Risky scopes

A risky scope is never included by Quick fill or by the default form. The
token page shows it in a separate group with a warning. Give it only to a
token that needs it.

| Scope | Tool | HTTP | Risk |
|---|---|---|---|
| `keys:revoke` | `revoke_api_key` | `POST /v1/project/api-keys/{api_key_id}/revoke` | Cannot be undone. Each game client or server that uses the key stops at once. Needs `keys:read`. A key of another Game Project, a key with no Game Project, or a revoked key is 404. A secret key needs permission to manage secret keys for the person who made the token. |
| `keys:secret` | `create_secret_api_key` | `POST /v1/project/secret-api-keys` | Returns a secret. The `ggs_` key value is in the result one time only. It is never in `list_api_keys`, `get_token`, the audit log, or the server log. Needs permission to manage secret keys for the person who made the token. Same label, scope, and `MANAGEMENT_MAX_PROJECT_API_KEYS` rules as `create_api_key`. |
| `steam:write` | `set_steam_auth` | `PUT /v1/project/steam-auth` | Cannot be undone: the old Web API key is lost. Takes `app_id` and `web_api_key` (input only; same checks as the dashboard). Leave `web_api_key` out to keep the stored key and change the App ID; this is refused when no key is stored. The key is sealed as in the dashboard and is never in a result, the audit log, or the server log. Returns the `get_steam_auth` shape. |
| `steam:write` | `clear_steam_auth` | `DELETE /v1/project/steam-auth` | Cannot be undone: removes the App ID and the Web API key, and Steam sign-in stops. Returns the `get_steam_auth` shape. |
| `players:invite` | `invite_player` | `POST /v1/project/players/invite` | Sends an email. Takes `email`. The address gets a link to accept the invite. Same invite limits as the dashboard (each inviter in an hour, each email domain in a day; see `set_project_invite_limit`): the answer is 429 with `Retry-After` when the limit is used. 409 when an invite for the address is pending, 422 for an invalid address. The inviter in the email and in the limit is the person who made the token. The result has `invite_id` and `expires_at`, and `email` only with `players:pii`. The audit row has no address. Needs `players:read`. |
| `players:invite` | `link_player` | `POST /v1/project/players/{player_id}/link` | Sends an email. Same as `invite_player`, for one existing player: when the invite is accepted, the address and its account are bound to that player, and an open invite for the player is replaced. 404 for a player of another Game Project, 409 when another player has the address. |

Each risky tool description starts with its risk ("Cannot be undone.", "Sends an email." or
"Returns a secret."), so the agent sees the risk in its tool list.

### What a token can do

- A token works for one Game Project only. No URL and no tool argument takes a
  project ID.
- A token never has more rights than the person who made it. If that person
  leaves the Account Tenant or becomes a `member`, the token stops (HTTP 403)
  and the token list shows it as **inactive**. If that person is disabled, the
  token also stops (HTTP 403), but the list does not show it as inactive.
- An expired token gets HTTP 401 with the message `token expired`. Make a new
  token. Expired tokens stay in the list for 30 days.
- A management token does not work on the player API, and an API key does not
  work on `/v1/project` or `/mcp`.
- No operation returns a secret, a player email, a display name, an IP
  address, or storage content.
- Text that a game client sent in a matchmaking ticket is shown only when it is
  a short identifier (letters, digits, `_`, `-`, `.`, up to 64 bytes). Other
  text is replaced by a marker with its length. This stops a player from
  writing instructions to your agent.

### Revoke a token

Select **Revoke** in the token list. The creator can revoke their own token.
Other people need permission to manage secret API keys. The token stops within
`MANAGEMENT_TOKEN_CACHE_TTL` (default 2 seconds).

## Direct HTTP use

Send the token in the `Authorization` header. A request needs no project ID:
the token selects the Game Project.

Read the setup problems:

```sh
curl -H "Authorization: Bearer ggm_..." https://<host>/v1/project/health
```

Change the remote config. First read the newest revision:

```sh
curl -H "Authorization: Bearer ggm_..." https://<host>/v1/project/remote-config
```

The answer has `config` and `revision`. Send that revision as
`expected_revision`:

```sh
curl -X PUT https://<host>/v1/project/remote-config \
  -H "Authorization: Bearer ggm_..." \
  -H "Content-Type: application/json" \
  -d '{"config":{"max_players":8,"motd":"Welcome"},"expected_revision":3}'
```

The answer has the new `revision`. A stale `expected_revision` answers 409.

The full reference is the `Project Management` tag in `openapi.yaml`, with the
security scheme `ManagementToken`.

### Errors

| Case | HTTP | MCP |
|---|---|---|
| Bad, expired, or revoked token | 401, `WWW-Authenticate: Bearer` | HTTP 401 on `/mcp` |
| Access ended (Account Tenant, creator, or Game Project disabled) | 403 | HTTP 403 on `/mcp` |
| Over the token rate | 429 | HTTP 429 on `/mcp` |
| `Origin` header for a different host | 403 | HTTP 403 on `/mcp` |
| Body larger than 131072 bytes | 413 | HTTP 413 on `/mcp` |
| Missing scope | 403 | `unknown tool` |
| Missing permission of the creator | 403 | tool result with `isError` |
| Wrong arguments | 422 | tool result with `isError` |
| Unknown argument | 422 | JSON-RPC invalid-params error |
| Resource of a different Game Project | 404 | tool result with `isError` |
| Stale `expected_revision` | 409 | tool result with `isError` |
| Limit reached (API keys, origins) | 409 | tool result with `isError` |

- An operation error is a problem+json body
  (`application/problem+json`). In MCP, the tool result text is that body.
- The token checks (401, the 403 for access ended, 429, and the `Origin`
  check) answer plain text, before the operation runs.
- `list_revisions` for a leaderboard of a different Game Project answers 200
  with an empty `revisions` list.

## For operators (self-host)

The feature is off by default. Set these environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `FEATURE_MANAGEMENT_API_ENABLED` | `false` | `true` mounts `POST /mcp` and `/v1/project`, and shows the token page. |
| `MANAGEMENT_TOKEN_MAX_EXPIRY_DAYS` | `365` | Longest lifetime a person can give a token. |
| `MANAGEMENT_TOKEN_RATE_PER_SECOND` | `5` | Requests per second for each token. |
| `MANAGEMENT_TOKEN_RATE_BURST` | `20` | Burst for each token. |
| `MANAGEMENT_TOKEN_CACHE_TTL` | `2s` | How long each server process keeps a successful token check. `0s` turns the cache off. |
| `MANAGEMENT_AUTH_FAILURES_PER_MINUTE` | `10` | Failed authentications each minute for each client IP. |
| `MANAGEMENT_AUTH_FAILURE_BURST` | `10` | Burst of failed authentications for each client IP. |
| `MANAGEMENT_MAX_PROJECT_TOKENS` | `20` | Active tokens for each Game Project. |
| `MANAGEMENT_MAX_PROJECT_API_KEYS` | `20` | `create_api_key` and `create_secret_api_key` are refused when the Game Project has this many active API keys. The dashboard is not limited by it. |
| `LEADERBOARD_MAX_PER_PROJECT` | `1000` | `create_leaderboard` and `restore_leaderboard` answer 409 when the Game Project has this many live leaderboards. The dashboard has the same limit. A platform admin can change it for one Account Tenant (quota axis `leaderboards`). |
| `CORS_MAX_PROJECT_ORIGINS` | `20` | Origins for each Game Project, for `set_allowed_origins` and the dashboard form. |

The old `MCP_*` and `FEATURE_MCP_ENABLED` names are ignored. If you set only
the old names, the feature is off and the limits are the defaults.

Notes:

- The rate limit is kept in each server process. With two app hosts, the
  effective limit for a token can be two times the configured value.
- Each token has two rate buckets with the same rate: one for `/mcp` and one
  for `/v1/project`. A tool call takes one unit from each bucket, because the
  server runs the tool as an internal `/v1/project` request. A direct HTTP
  request takes one unit from the `/v1/project` bucket.
- Each server process keeps a successful token check for
  `MANAGEMENT_TOKEN_CACHE_TTL`. A tool call checks the token two times (for
  `/mcp` and for the inner `/v1/project` request), so the second check uses
  the cache. A revoked token, a disabled Account Tenant or creator, and a
  deleted Game Project take effect within this time on each process. The
  expiry time, the creator's role, and the rate limit are checked on each
  request.
- Failed authentications use a per-IP bucket that `/mcp` and `/v1/project`
  share. Correct tokens never use it, so they keep working from an IP that
  sent bad tokens. Behind a load balancer, set `TRUSTED_PROXY_HEADER` and
  `TRUSTED_PROXY_CIDRS` so the server sees the client IP.
- `/mcp` and `/v1/project` refuse a request that has an `Origin` header for a
  different host. Coding agents and scripts send no `Origin` header.
- The platform admin **Server settings** page shows the current values.
