# Management API v2: more operations, finer token permissions

Status: plan written 2026-11-21. Stages 1 to 4 and 6 are done. Stage 5
(tenant token) is not started. Depends on PR #107
(`docs/management-api-plan.md`), which was merged first.

This plan adds operations to the project management API. Each operation serves
three clients with no difference: plain HTTP (`/v1/project/...`), the coming
CLI, and MCP agents (`POST /mcp`, through huma's `mcp` package). It also makes
token permissions finer, so a person can give a token exactly the access it
needs.

This document is self-contained. An agent can do a stage from it without other
context. The rules of `docs/management-api-plan.md` and `archive/mcp-plan.md`
stay in force unless this document changes them.

## Goal

- A person on the CLI and an AI agent can do most project work without the
  dashboard: keys, players, limits, matchmaking, fleets, and later tenant work.
- A token gets only the access that it needs: a read scope and a write scope
  for each resource, and separate opt-in scopes for risky operations.
- Platform-admin operations stay out of scope.

## Decisions

Made with the user on 2026-11-21.

| Topic | Decision |
|---|---|
| Same operations for all clients | Every operation is an HTTP operation and an MCP tool. No operation is for one client only. Control is by scope, not by client. A per-client limit is not a security boundary: an agent with a shell can run the CLI. |
| Read access | Read scopes for each resource. Before this plan, every token could use every read. |
| Old tokens | No migration. A token made before read scopes has no read access. Make a new token. |
| Risky operations | An operation that cannot be undone, returns a secret, or sends email gets its own opt-in scope. Opt-in scopes are never in a preset. The token page shows them in a separate group with a warning. The operation runs at once (no approval step). |
| Secrets | No operation returns a stored secret. One exception: `create_secret_api_key` returns the new key value one time, behind the opt-in scope `keys:secret`. A secret that a token writes (the Steam Web API key) is never returned. |
| Player data | Without `players:pii`: player ID, external ID, status flags, and timestamps. With the opt-in scope `players:pii`: email and display name too, unchanged, in their own fields. The tool description says that these fields are text that players wrote. The client text filter is not used for them, because it hides almost every real email and name. |
| Player delete | `request_player_delete` is allowed with `players:write`, because `cancel_player_delete` undoes it during `PLAYER_DELETE_GRACE_PERIOD`. This replaces the old "no player delete" rule. |
| Player ban | Out of the project token. A ban applies to the whole Account Tenant (through the linked player account). It goes to the tenant token stage. |
| Fleets | Reads and fleet writes are in. Fleet delete gets a restore operation, so it can be undone. Allocate and deallocate are risky (opt-in scope `allocations:write`). All fleet operations need `FEATURE_FLEET_ENABLED` and the `dedicated_servers` feature grant, as in the dashboard. |
| Tenant token | A later stage of this plan, with its own security review. Team role changes, token creation, and tenant disable/enable stay out, because a token must not change the rights of its creator or of the tenant. |
| Platform admin | No platform-admin operation (tier, feature grants, tenant quota and rate overrides, signups, change request approval). |
| Approval flow | Not in this plan. The agreed design stays in `archive/mcp-plan.md` "Later". |
| Unavailable options | The token page shows an option that the person cannot give, disabled, with the reason. Before this plan, the page hid it. |
| Change after creation | No edit. A token's access never changes after it is made. Each token in the list has a "New token like this" link that fills the form with the same access. |

### Old rules that this plan changes

| Old rule (`archive/mcp-plan.md`) | New rule | Why |
|---|---|---|
| Every token can use every read. | Read scopes for each resource. Old tokens lose their reads; make a new token. | A token for one task does not need to see keys, players, or fleets. |
| No operation that cannot be undone. | Allowed behind an opt-in scope that is never in a preset. | A CLI user must be able to revoke a key or deallocate a server. The scope makes the risk a clear choice. |
| No operation returns a secret. | One exception: a new secret API key, behind `keys:secret`. | CI and deploy scripts need secret keys. A stored secret is still never returned. |
| No player data except IDs and flags. | Email and display name behind `players:pii`. | Support work on the CLI needs them. The opt-in scope and the warning keep agents safe by default. |
| No player delete. | Allowed with `players:write`. | It has an undo during the grace period. |
| No fleet operation. | Reads and fleet writes; allocate and deallocate behind an opt-in scope. | Dedicated server users need it, and fleet delete gets a restore. |
| No tenant-level token. | A later stage of this plan. | Project create and list, and team invites, need it. |

## Design

### Operation classes

Each operation is in one class. The class decides the scope and the checks.

1. **Reversible, project-level, no secret.** A normal scope. Can be in a
   preset.
2. **Risky:** cannot be undone, returns a secret, writes a secret, or sends
   email. An opt-in scope. Never in a preset.
3. **Tenant-level.** Needs the tenant token (stage 5).
4. **Out:** platform-admin operations, team role changes, token creation,
   tenant disable and enable, project delete.

### Scopes

A scope is `<resource>:<action>`. Each scope maps to one Casbin pair that the
creator must pass, in `managementtoken.WriteScopes` (rename it to `Scopes`).
The creator limit stays: the token page offers only the scopes whose pair the
creator passes, and each request checks the pair again.

| Scope | Class | Operations | Creator pair |
|---|---|---|---|
| `config:read` | 1 | `get_remote_config`, `list_revisions` (remote config), `get_steam_auth` | `(project, manage)` |
| `config:write` | 1 | `set_remote_config`, `rollback_remote_config` | `(project:<id>:config, update)` |
| `origins:write` | 1 | `set_allowed_origins` | `(project:<id>:config, update)` |
| `leaderboards:read` | 1 | `list_leaderboards`, `get_leaderboard`, `list_revisions` (leaderboard) | `(project, manage)` |
| `leaderboards:write` | 1 | create, update, rollback, delete, restore leaderboard | `(project:<id>:leaderboard, manage)` |
| `keys:read` | 1 | `list_api_keys` | `(project, manage)` |
| `keys:create` | 1 | `create_api_key` (publishable) | `(api_key:publishable, manage)` |
| `keys:write` | 1 | `update_api_key_label`, `update_api_key_features` | the key type's pair, as `authorizeAPIKeyManagement` |
| `keys:revoke` | 2 | `revoke_api_key` | the key type's pair |
| `keys:secret` | 2 | `create_secret_api_key` | `(api_key:secret, manage)` |
| `players:read` | 1 | `list_players`, `get_player` | `(project:<id>:players, manage)` |
| `players:pii` | 2 | adds `email` and `display_name` to the player reads | `(project:<id>:players, manage)` |
| `players:write` | 1 | `set_player_disabled`, `request_player_delete`, `cancel_player_delete` | `(project:<id>:players, manage)` |
| `players:invite` | 2 | `invite_player`, `link_player` (send email) | `(project:<id>:players, manage)` |
| `matchmaking:read` | 1 | `matchmaking_ticket_trace`, `get_matchmaking_queue` | `(project:<id>:matchmaker, read)` |
| `limits:read` | 1 | `get_project_limits` | `(project, manage)` |
| `limits:write` | 1 | `set_project_invite_limit`, `set_project_storage_limit` | `(project, manage)` |
| `steam:write` | 2 | `set_steam_auth`, `clear_steam_auth` (the old key is lost) | `(project:<id>:config, update)` |
| `fleet:read` | 1 | `list_fleets`, `get_fleet`, `list_allocations`, `get_allocation`, `list_allocation_events` | `(project:<id>:fleet, read)`, `(project:<id>:allocation, read)` |
| `fleet:write` | 1 | `create_fleet`, `update_fleet`, `delete_fleet`, `restore_fleet` | `(project:<id>:fleet, manage)` |
| `allocations:write` | 2 | `allocate_server`, `deallocate_server` | `(project:<id>:allocation, allocate)`, `(…, deallocate)` |

Notes:

- The read pairs were checked in `internal/rbac`. Casbin matches the action
  exactly, and no role has `(project:<id>:config, read)` or
  `(project:<id>:leaderboard, read)`, so `config:read` and
  `leaderboards:read` use `(project, manage)`, the check of the dashboard
  pages today.
- `players:read` uses `(players, manage)`, not `read`: the dashboard players
  pages are open to `(project, manage)` (tenant_admin and up), and tenant_admin
  has `(players, manage)` but not `(players, read)`, so a `read` pair would
  refuse a person who can open the page.
- A risky scope has two more table fields: `Label` (the sentence of its box on
  the token page) and `Needs` (a scope that the token must also have). The
  token page text "Needs read access to ..." and the refusal on POST come from
  them, so a later risky scope is a table entry only.
- Check the pair of each later read scope in `internal/rbac`. If a read pair does
  not exist, use the pair that the dashboard page checks today (for most
  project pages that is the route gate `(project, manage)`). The pair must
  never be weaker than the dashboard.
- `project_health_check` stays open to every token. It leaves out each section
  that needs a read scope the token does not have (for example the API key
  list without `keys:read`).
- One operation can need two pairs (`fleet:read` for fleets and
  allocations). Then the table row holds both, and each operation checks its
  own pair.
- `fleet:read` uses `(fleet, read)`: the dashboard fleet pages check it
  (`requireProjectRead(ProjectFleetObject)`), and the scope pair must not be
  weaker than the page. The allocation operations also set `projectOp.pair`
  to `(allocation, read)`, the check of the dashboard allocation pages, so
  each request checks it. The dashboard read pages do not check the
  `dedicated_servers` grant; the operations do, as the dashboard writes do.
- The token page is described in "Token page".

### Token page

The page stays at
`/v1/control-panel/tenants/{tenantID}/projects/{projectID}/management-tokens`.
It is server-rendered with no JavaScript, as all dashboard pages.

**New token form.** Label, expiry days, then an access table with one row for
each resource and one radio group in each row:

```
  Access                     No access   Read   Read and write
  Remote config and origins     ( )       (•)        ( )
  Leaderboards                  ( )       (•)        ( )
  API keys                      ( )       (•)        ( )
  Players (IDs and status)      ( )       (•)        ( )
  Matchmaking                   ( )       (•)         —
  Project limits                ( )       (•)        ( )
  Fleets                        Not available: dedicated servers are
                                not enabled for this Account Tenant.

  Quick fill: [Read only] [Read and write]

  Risky access (never included by Quick fill)
  [ ] Revoke API keys. Cannot be undone; a game that uses the key stops.
  [ ] Create secret API keys. The key value is returned to the token user.
  [ ] Change the Steam Web API key. The old key is lost.
  [ ] See player email and display name. They go into agent logs and to the AI model provider.
  [ ] Invite and link players. Sends email.
  [ ] Start and stop game servers. Stopping ends a live match.
```

- Each row maps to scopes: "Read" gives `<resource>:read`, "Read and write"
  gives the read scope and the class 1 write scopes of that row
  (`config:write` and `origins:write` for the first row). A row with no write
  scope has no "Read and write" option.
- Each risky box gives one class 2 scope.
- Default: "Read" in each row, no risky box. A new token is read-only unless
  the person changes it.
- "Quick fill" buttons are links (`?fill=read_only`, `?fill=read_write`) that
  reload the form filled in. They never check a risky box.
- An option that the person cannot give is disabled, with the reason:
  - "Your role does not allow this." The creator fails the Casbin pair.
  - "Not enabled for this Account Tenant." A feature grant is missing (for
    example `dedicated_servers` for fleets and servers).
  - "Turned off on this server." A server switch is off (for example
    `FEATURE_FLEET_ENABLED`).
- Each risky box that needs another access says so, for example "Needs read
  access to Players". The server refuses the form when the dependency is
  missing: `players:pii` and `players:invite` need `players:read`;
  `allocations:write` needs `fleet:read`; `keys:revoke` needs `keys:read`.
- The form sends `access_<resource>=none|read|write` and `risky=<scope>`. The
  server turns them into scopes and checks each Casbin pair, each feature
  grant, and each switch again. It refuses a disabled option.

**After creation.** The token value is shown one time (as today), with the
access listed in words.

**Token list.** Access in words, for example "Config: write · Leaderboards:
read · Risky: revoke keys". Risky access has a warning badge. When the creator
has lost a right, the row says so, for example "2 permissions inactive:
creator lost access". Each row has "New token like this" (a link that fills
the form with the same access) and "Revoke".

**No edit.** A token's scopes never change after it is made. To change
access, the person makes a new token and revokes the old one.

### Token introspection

A new operation `get_token` (`GET /v1/project/token`), open to every token. It
returns the token ID, label, project ID and name, tenant ID, scopes, expiry
time, and the scopes that the creator can still use now. A CLI uses it for
`whoami`. An agent uses it to see why a tool is missing.

### Paging

Every list operation takes `limit` and `cursor` and returns `next_cursor`, as
`GET /v1/leaderboards` does. This replaces the fixed 50 rows and `truncated`.
Lists: leaderboards, API keys, players, fleets, allocations, allocation
events, matchmaking queue. The page size has a default and a maximum, as the
other paged routes.

### Shared write functions and audit

Each token write must write an audit row with the actor
`management_token:<id>`, in the same transaction as the change. Today these
dashboard writes have no audit row or have one outside the transaction:
player disable, player invite and link, project create, and the fleet writes
(audit after the change). For each operation in this plan:

- Move the write into a shared function that does no authorization and takes
  a `projectadmin.Actor`, like `internal/projectadmin` does. Put player writes
  in `internal/projectadmin/players.go`, key writes in `apikeys.go`, limits in
  `limits.go`. Fleet writes stay in `internal/fleet`, with an actor argument.
- Change the dashboard handler to call the same function. The dashboard then
  gets the same audit rows.

### MCP usability

- The tool list grows from 16 to about 45 tools. `tools/list` already hides
  tools outside the token's scopes, so a narrow token sees a short list.
- Each class 2 tool description starts with "Cannot be undone." or "Sends an
  email." or "Returns a secret.", because huma v2.41.0 has no tool annotations
  (`destructiveHint`).
- Update `mcpInstructions` to tell the agent to call `get_token` when a tool
  is missing.
- The player PII fields are separate JSON fields, never mixed into other text,
  and the tool description says that they are untrusted player text.

### Tenant token (stage 5)

A new token type for one Account Tenant. Design questions to settle in the
stage 5 review, before code:

- Prefix (`ggt_`), table, and token page (tenant settings).
- How it selects a project for project operations: paths
  `/v1/projects/{project_id}/...` registered from the same `projectOps` table,
  or a project selector argument. The project token keeps `/v1/project/...`.
- Tenant scopes: `projects:read`, `projects:create`, `team:read`,
  `team:invite` (class 2, sends email), `change_requests:write`,
  `custom_token:write`, `players:ban` (class 2, tenant-wide).
- Project scopes for all projects or for a list of projects.
- The creator limit for checks that are not Casbin. Tenant disable and enable
  check the membership role, so they stay out.
- Out: team role changes, member removal, token creation, tenant disable and
  enable, project delete.

### Out of scope

- All platform-admin routes and operations.
- An approval flow for risky operations.
- A per-client limit on a token.
- Player storage content, IP addresses, join codes, and session data.

## Done when

- A token with only `leaderboards:read` sees only the leaderboard read tools
  in `tools/list`, and gets 403 on `GET /v1/project/api-keys`.
- A token made before read scopes has no read access. The release notes tell
  people to make a new token.
- A class 2 scope is in no preset, and the token page shows it in a separate
  group with a warning.
- Each new write operation writes one audit row with `management_token:<id>`,
  in the same transaction, and the dashboard write of the same action writes
  the same row.
- `get_player` without `players:pii` returns no email and no display name.
- `delete_fleet` followed by `restore_fleet` gives back the same fleet.
- `create_secret_api_key` returns the value one time; `list_api_keys` never
  returns a value.
- No operation is reachable by a platform-admin-only path or scope (test on
  the `projectOps` table, as today).
- `openapi.yaml`, the MCP drift test, and `docs/management-api.md` list every
  operation.

## Work stages

Do the stages in order. Write failing tests first. After each stage:
`make lint`, `make test`, `make test-integration`, and the `controlpanel`,
`httpapi`, and `migrate` suites. Mark each task here when it is done.

### Stage 1: read scopes, introspection, paging

- [x] Check the read pairs in `internal/rbac` and fix the scope table above.
- [x] Scope table: rename `WriteScopes` to `Scopes`, add the read scopes and the class of each scope.
- [x] `projectOps`: each read row gets its read scope. `list_revisions` needs `config:read` or `leaderboards:read`, by resource.
- Dropped: ~~Migration: add all read scopes to every existing `management_tokens` row.~~ Old tokens lose their reads; make a new token. The token page form selects "Read" in each row by default, so a new token gets each read scope that its creator can give and no write scope.
- [x] Token page: the access table, Quick fill links, the risky group (empty in this stage), disabled options with the reason, server-side mapping from form fields to scopes. See "Token page".
- [x] Token list: access in words, the inactive count, "New token like this".
- [x] Tests: a disabled option is refused on POST; Quick fill never sets a risky scope; "New token like this" fills the same access; the default form makes a read-only token.
- [x] `project_health_check`: leave out each section that the token cannot read.
- [x] `get_token` operation.
- [x] Paging for `list_leaderboards` and `list_api_keys`.
- [x] `mcpInstructions`: mention `get_token`.
- [x] Tests: a narrow token's tool list; 403 for a missing read scope.
- [x] Tests: paging.

### Stage 2: keys, players, limits, matchmaking (class 1)

- [x] `update_api_key_label`, `update_api_key_features` (`keys:write`). Move `updateAPIKeyLabel` and `updateAPIKeyManagedScopes` from `controlpanel/key_management.go` to `projectadmin/apikeys.go`.
- [x] `list_players`, `get_player` (`players:read`), search by player ID and external ID, paged. Add `email` and `display_name` only with `players:pii`.
- [x] `players:pii` in the class 2 group of the token page.
- [x] `set_player_disabled`, `request_player_delete`, `cancel_player_delete` (`players:write`) in `projectadmin/players.go`, with audit rows. The dashboard player disable gets an audit row too.
- [x] `get_project_limits`, `set_project_invite_limit`, `set_project_storage_limit` (`limits:read`, `limits:write`) in `projectadmin/limits.go`. Same bounds as the dashboard. Make the storage limit write and its audit row one transaction.
- [x] `get_matchmaking_queue` (`matchmaking:read`), the bucket view of the dashboard queue page. `matchmaking_ticket_trace` already uses `matchmaking:read`.
- [x] `get_steam_auth` (`config:read`): App ID and whether a key is set. Never the key.
- [x] Tests for each operation: scope refusal, creator pair refusal, another project's resource is 404, audit row.

### Stage 3: fleets (class 1)

- [x] `list_fleets`, `get_fleet`, `list_allocations`, `get_allocation`, `list_allocation_events` (`fleet:read`), paged.
- [x] `create_fleet`, `update_fleet`, `delete_fleet` (`fleet:write`).
- [x] Fleet restore: query, `restore_fleet` operation, and a restore button on the dashboard fleet list.
- [x] Fleet writes take an actor and write the audit row in the transaction, for the dashboard and for tokens.
- [x] The operations are registered only when `FEATURE_FLEET_ENABLED` is on, and each one checks the `dedicated_servers` feature grant.
- [x] Tests: off switch hides the tools; no grant gives 403; delete then restore.

### Stage 4: risky operations (class 2)

- [x] `revoke_api_key` (`keys:revoke`). Move `revokeAPIKey`, the RBAC role removal, and the cache bust into `projectadmin/apikeys.go`.
- [x] `create_secret_api_key` (`keys:secret`): returns the value one time. Same key limit as `create_api_key`.
- [x] `set_steam_auth`, `clear_steam_auth` (`steam:write`): the key is input only and is sealed as today.
- [x] `invite_player`, `link_player` (`players:invite`): same throttle as the dashboard, plus an audit row.
- [x] `allocate_server`, `deallocate_server` (`allocations:write`).
- [x] Each tool description starts with the risk sentence.
- [x] Token page: add each risky box with its sentence and its dependency check.
- [x] Tests: no preset has a class 2 scope; the token page shows the warning; the secret is not in any list or log.

### Stage 5: tenant token

- [ ] Write the stage 5 design (see "Tenant token") and get a security review before code.
- [ ] Then add its tasks here.

### Stage 6: documents

- [x] `docs/management-api.md`: scope table, classes, presets, and each new operation.
- [x] Release notes: read scopes (old tokens lose reads; make a new token), the new operations, and the changed rules.
- [x] `openapi.yaml` (`make openapi`).

## Risks

- **Tool count.** About 45 tools for a token with all scopes can use a large
  part of an agent's context. The scope filter is the control. Stage 1 must
  make narrow tokens easy to create.
- **Opt-in scopes are still powerful.** A person can give `keys:revoke` to an
  agent, and the agent can then break a live game. The warning on the token
  page and the tool description are the only protections, because there is
  no approval flow.
- **PII in agent transcripts.** With `players:pii`, player email can go into
  an agent's logs and into the model provider's systems. The token page
  warning must say this.
- **Inline dashboard code.** Many dashboard writes are inline in
  `internal/controlpanel`. Moving them to shared functions changes dashboard
  code too, so each stage must run the `controlpanel` suite.
- **Old tokens.** There is no read scope migration. A token made before read
  scopes loses its reads and must be made again. The previous release checks
  a scope only by membership, so a new token with read scopes still works
  there.

## Files to change

- `internal/managementtoken/managementtoken.go` (scope table and classes)
- `internal/httpapi/project_ops.go`, `project_read.go`, `project_write.go`, `management_mcp.go`
- `internal/projectadmin/` (`apikeys.go`, new `players.go` and `limits.go`)
- `internal/fleet/` (actor and restore)
- `internal/controlpanel/` (token page, key, player, limit, and fleet handlers)
- `internal/db/queries/` and a new migration
- `tests/integration/managementapi/`, `tests/integration/controlpanel/`, `tests/integration/migrate/`
- `docs/management-api.md`, `openapi.yaml`

## Release notes (draft)

This text is for the CHANGELOG section that is written when the release is tagged.

#### Breaking

- **Management tokens now need read scopes.** Before, every token could use
  every read. Now each resource has a read scope (`config:read`,
  `leaderboards:read`, `keys:read`, `players:read`, `matchmaking:read`,
  `limits:read`, `fleet:read`). A token made before this release has only its
  write scopes and no read access. Make a new token.

#### Added

- Add the management API operations for keys: `list_api_keys` is now paged,
  and `update_api_key_label`, `update_api_key_features`, `revoke_api_key`, and
  `create_secret_api_key` are new.
- Add the operations for players: `list_players`, `get_player`,
  `set_player_disabled`, `request_player_delete`, `cancel_player_delete`,
  `invite_player`, and `link_player`.
- Add the operations for limits: `get_project_limits`,
  `set_project_invite_limit`, and `set_project_storage_limit`.
- Add the operations for matchmaking and Steam: `get_matchmaking_queue`,
  `get_steam_auth`, `set_steam_auth`, and `clear_steam_auth`.
- Add the operations for fleets and allocations: `list_fleets`, `get_fleet`,
  `create_fleet`, `update_fleet`, `delete_fleet`, `restore_fleet`,
  `list_allocations`, `get_allocation`, `list_allocation_events`,
  `allocate_server`, and `deallocate_server`. They need
  `FEATURE_FLEET_ENABLED` and the `dedicated_servers` feature grant.
- Add `get_token` (`GET /v1/project/token`). It shows the scopes of a token
  and the scopes that its creator can still use.
- Add `limit`, `cursor`, and `next_cursor` to every list operation. This
  replaces the fixed 50 rows.
- Add `FLEET_ALLOCATION_MAX_CAPACITY` (default `100`), the largest capacity of
  one manual allocation.

#### Changed

- An email address with an IP address as its domain (`a@[127.0.0.1]`) is not
  valid for signup, invites, and the other address checks. Login does not
  check the address form.
- Risky operations have their own opt-in scopes. No preset includes them, and
  the token page shows them in a separate group with a warning:
  `keys:revoke`, `keys:secret`, `steam:write`, `players:pii`,
  `players:invite`, and `allocations:write`. Some of them need a read scope on
  the same token.
- A token can now do things that the old rules did not allow, each behind a
  scope: revoke an API key (`keys:revoke`), create a secret API key and see its
  value one time (`keys:secret`), set or clear the Steam Web API key
  (`steam:write`), see player email and display name (`players:pii`), invite
  and link players (`players:invite`), and start and stop game servers
  (`allocations:write`). The values of existing keys and the Steam Web API key
  are still never returned.
- A token can request a player delete with `players:write`. The deletion
  starts after `PLAYER_DELETE_GRACE_PERIOD`, and `cancel_player_delete` undoes
  it before then.
- `project_health_check` leaves out each section that the token has no read
  scope for.
- The management token page has an access table with **No access**, **Read**,
  and **Read and write** for each resource, **Quick fill** links, a group for
  risky access, and a reason on each option that you cannot give. A new token
  is read-only by default. Each token in the list shows its access in words, a
  count of inactive permissions when its creator lost a right, and a **New
  token like this** link. A token never changes after you make it.
- The dashboard writes an audit row in the same transaction as the change for
  player disable (new row), invite, link, limits, and fleet changes. If the
  audit row fails, the action now fails too.
- The dashboard fleet list has a **Restore** button for a deleted fleet.
- Plugin fleet config values are now sealed at rest with the credential key
  (the key that seals the Steam Web API key). The keys stay plain. An older
  row is sealed on its next update. To seal the existing plugin fleets after
  the upgrade, save each one once: open it on the dashboard and click **Save
  changes** (empty values keep the stored ones), or call `update_fleet` with its name
  only. Database backups made before that keep the plain values. After a
  rollback to an older release,
  enter the plugin config again: the older release cannot read sealed values.
  When a sealed value does not open (the credential key was lost or
  changed), allocation, plugin cleanup, and an update that keeps the value
  are refused (422 on update) until the plugin config is entered again.
- The dashboard does not show plugin fleet config values. The fleet list
  shows the keys only. The edit page shows each key with an empty value: an
  empty value keeps the saved value, a new value replaces it, and a cleared
  key removes it. Clear all keys to clear the config. The same key in two
  rows is refused.
- The dashboard allocation form is limited by `FLEET_ALLOCATION_MAX_CAPACITY`.
- A fleet name and backend are at most 256 bytes, valid UTF-8, and no NUL, in
  the dashboard and in the management API (over the API, invalid UTF-8 in the
  JSON body gives 400). The backend must be `agones` or a
  plugin backend. Before, a long name gave an error 500.
- A list operation answers 422 for a cursor that it did not return. Before,
  `list_api_keys`, `list_players`, and `list_allocation_events` started again
  at the first page.
- The cursor of `get_matchmaking_queue` is now sealed. It does not show
  bucket text.
- A fleet backend for a plugin is now named `plugin:<name>`. A fleet stored as
  bare `plugin` keeps working. Allocation on plugin backends now works (it
  answered 409 before). A legacy fleet with another `plugin*` backend value
  without the `plugin:` prefix (for example `pluginx`) now gets a 422 from
  management token fleet operations. A new fleet cannot have an empty plugin
  name (`plugin:`).
- `null` for `scopes` in `update_api_key_features` and for `origins` in
  `set_allowed_origins` now gives 422. `[]` still clears the list.
- Token labels and API key labels (dashboard, token API, and sign-up) refuse
  control and text-direction characters. The API key label is at most 200
  characters.
- NUL in a fleet config and in `fleet` and `external_id` of
  `allocate_server` gives 422. NUL and invalid UTF-8 in the leaderboard
  `cursor` give 422 over HTTP. A `fleet` that
  is too long also gives 422.
- The token page, the token list, and the Deleted fleets list fit on a narrow
  screen.
- The invite limit `domain_per_day` counts invites for the whole Game Project
  in a day, not for each email domain. The text now says so.
- The **Server settings** page shows `FLEET_ALLOCATION_MAX_CAPACITY`.
- Validation errors no longer include the refused value (`errors[].value`).
  This is for every API operation and MCP tool. Only the 409
  `ticket_already_active` keeps the active ticket id in `errors[].value`.
- A request body that cannot be decoded (JSON that does not parse, or a
  value that does not fit its field, such as `1e30` for an integer) now
  gives the fixed message `request body could not be decoded`. Before, the
  message quoted the first bad byte or the number.
- `POST /v1/auth/delete/cancel` with a bad email still answers 422 at
  `body.email`, but the message is now `expected a valid email address`.
  Before, the message quoted part of the address.
- A JSON request body with invalid UTF-8, or with a lone surrogate escape
  such as `"\ud800"`, now answers 400 with the message `request body is not
  valid UTF-8 text`, for every API operation. An MCP tool call with such text
  in any argument gets a JSON-RPC invalid-params error with that message.
  Before, most fields stored the text with U+FFFD in its place.
- The `auth.signup`, `auth.login`, `auth.custom_token`, and `auth.steam`
  audit rows now name the player by its id in `target`. Before, `target` held
  the player email or the external id (a custom token external id can be an
  address). Old rows keep the old value.
- The `create_secret_api_key` tool description and the docs now say that the
  MCP result holds the key value two times (in the text and in
  `structuredContent`).
- The dashboard allocate form now also needs the feature grant of the fleet
  backend (`fleet_agones_backend` or `fleet_plugin_backend`), as fleet create
  and edit and the management API do. Without it, the form answers 403. The
  dashboard fleet and allocate forms also answer 403 for a fleet whose stored
  backend is not `agones`, `plugin` or `plugin:<name>`.
- The dashboard and the player account pages refuse a form post from another
  origin with 403 (`net/http.CrossOriginProtection`), in addition to the CSRF
  value. A request with no `Origin` and no `Sec-Fetch-Site` header still
  passes. The origin of `CONTROL_PANEL_BASE_URL` is trusted, so the forms work
  behind a proxy that changes the `Host` header when it is set; otherwise the
  proxy must keep the `Host` header.
- The dashboard and the player account pages show a message only from a
  fixed code in `?flash=<code>`. Free text in `?flash=`, `?created=` or
  `?error=` shows nothing, so a link cannot show a false message. Some
  messages no longer name the item or the address (for example "Invite sent."
  and "Fleet created."), and the player account pages say "provider" in place
  of the provider name in link and unlink messages.
- The platform audit rows for disabling and enabling a player account name the
  account by id only. They no longer hold the player email. Older rows keep
  their values.
