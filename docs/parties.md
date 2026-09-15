# Parties

A party is a group of up to eight players in one Game Project. It enters
matchmaking as one unit. Every member reaches the same match. A match can
contain several parties and solo players. A party does not guarantee a team.

## Player flow

Send the project API key in `Authorization: Bearer <key>` and the player token
in `X-Session-Token`. The key needs the `matchmaker` scope. Fleet modes also
need the `fleet` scope and the dedicated-server entitlement.

1. Create a party with `POST /v1/parties`:

   ```json
   {"settings":{"mode":"match_only","min_count":2,"max_count":4,"count_multiple":1}}
   ```

2. Invite a friend with `POST /v1/parties/{id}/invites`, or create a code with
   `POST /v1/parties/{id}/invite-codes`. These requests include
   `expected_version`. Friend invites also include `player_id`; code requests
   include `max_uses` (1–7).
3. Friends list their pending invites with `GET /v1/party-invites`, then accept
   with `POST /v1/party-invites/{id}/accept` and the `party_version` from the
   invite list as `expected_version`. A code recipient sends only
   `{"code":"<invite code>"}` to `POST /v1/parties/join`. Code joins do not need
   the party version. Codes ignore case and display hyphens. Invites and codes
   expire after five minutes.
4. Every member, including the leader, sends their own readiness:

   ```http
   PUT /v1/parties/{id}/members/me/ready
   ```

   ```json
   {"expected_version":4,"ready":true,"properties":{"numeric_properties":{"skill":1200}}}
   ```

5. The leader sends `POST /v1/parties/{id}/queue` with an `Idempotency-Key`
   header and `{"expected_version":6}`. Reuse the key to retry the same queue
   request. Use a new key for a new entry. A roster larger than `max_count`
   receives `409 party_exceeds_mode_capacity`.
6. Send `POST /v1/parties/{id}/heartbeat` every ten seconds with the current
   `expected_version`. Heartbeats preserve readiness and the party version.
7. Recover missed events with `GET /v1/parties/current` or
   `GET /v1/parties/{id}`. Each member has a `ticket_id`. Poll your own ticket
   with `GET /v1/matchmaker/tickets/{ticket_id}` to recover the match result.
8. Match commit clears readiness. Ready again, then let the leader send
   `POST /v1/parties/{id}/rematch` with a new `Idempotency-Key`,
   `expected_version`, and the expected `last_match_id`. The same party ID and
   current members enter a new queue entry. Solo fill never joins the party.

The generated [OpenAPI document](../openapi.yaml) is the API contract.

## Controls and errors

Only the leader can change idle settings, kick members, create or revoke
invites/codes, start/cancel a queue, request a rematch, or disband the party.
`DELETE /v1/party-invites/{id}` also lets the recipient decline an invite.
Members can only set their own readiness. Membership and setting changes clear
all readiness and increment `roster_version`. Mutations of an existing party,
except code joins, require the current party version; stale requests receive
`409 stale_version`.

Use `DELETE /v1/parties/{id}/queue` to cancel the entire entry. Individual
party-ticket cancellation is rejected. Active party members must leave before
creating solo tickets. Use `DELETE /v1/parties/{id}/members/me` to leave,
`DELETE /v1/parties/{id}/members/{player_id}` to kick, and
`DELETE /v1/parties/{id}` to disband. Include `expected_version` in each body.

Other stable errors are `party_full`, `not_leader`, `not_ready`, `party_busy`,
`ticket_already_active`, `party_already_active`, `party_member_must_leave`, and
`party_ticket_requires_leader_cancel`. Private reads return `404` to nonmembers.
Invalid or expired invites return `404 invalid_invite`. Code cooldowns return
`429 code_redemption_cooldown` and a `Retry-After` header.

## Presence and sessions

The database stores a deadline 30 seconds after the last valid heartbeat.
Expired members cannot queue or extend an expired heartbeat. The worker removes
expired members on its next scan (normally within five seconds). If queued,
it cancels the whole entry first. It promotes the connected member with the
earliest join time, then the lowest player ID. The remaining members must ready
again and the new leader must start the next queue.

Leaving a matched party changes future queues only. It does not leave, close,
or transfer the active game session. The match host comes from the matchmaker's
existing host selection, independently of the party leader. Match roster
`party_id` and `queue_entry_id` values remain after the party closes.

## Operator cutover

Party enqueue defaults to disabled (`PARTY_ENQUEUE_ENABLED=false`). There is
one entry claim path. Do not run old ticket-claim workers with the new workers.

1. Apply additive migration 45 while party enqueue is disabled. Keep the old
   runtime until the cutover. Migration 46 is the cutover, not an online step.
2. Pause all matchmaking enqueue and stop old workers. Allow in-flight claims
   to settle. Confirm `SELECT count(*) FROM matchmaking_tickets WHERE claim_id
   IS NOT NULL` returns zero.
3. Apply migration 46. It refuses unsettled claims, backfills each existing
   solo ticket into its own entry, then requires `entry_id` for every ticket.
4. Confirm there are no tickets with a null entry and every solo entry has one
   ticket. Start only the new runtime and workers. Resume solo enqueue.
5. Enable `PARTY_ENQUEUE_ENABLED` after solo and party smoke tests.

Do not roll the application back to ticket-only workers after party enqueue.
For a rollback, pause enqueue, stop workers, settle or cancel complete entries,
and confirm no live party entries remain before reverting the cutover.

### Recovery and limits

Workers lock parties before entries and tickets. Claims select whole entries
with `FOR UPDATE SKIP LOCKED`; the batch cap counts entries, so a batch may
contain up to eight times that many tickets. Grouping and commit cannot trim an
entry. Match, ticket, entry, and party state commit in one database transaction.
Commit checks member deadlines under the party lock even if the presence sweep
has not yet run.
Failed or expired entries return their party to idle and clear readiness.

Backend work runs outside that transaction. A durable resolution record exists
before session or fleet creation. Fleet allocation metadata includes
`ggscale.dev/resolution-id`; Agones copies it to the server. GC releases expired
uncommitted allocations and retries failed cleanup. Game-session orphans use
the existing session TTL. Resolution records remain for 24 hours after expiry
to catch delayed backend writes. Successful match commits remove their record.

Fleet plugins should implement `fleet.ResolutionCleaner` and preserve the
resolution label on allocated resources. The optional `CleanupResolution` RPC
is backward compatible. Older plugins can release a known resource reference, but return unsupported
for pending-resource recovery; the job retains the record and reports an error for operator action.
Upgrade those plugins before enabling party fleet queues. Agones credentials
need permission to list and delete GameServers in each configured namespace.

Codes contain 80 random bits (16 Crockford Base32 characters). Only SHA-256
hashes are stored. Ten failed redemptions per player/project within 15 minutes
cause a 15-minute player block. One hundred failed redemptions per source IP
across projects cause a 15-minute IP cooldown. Only trusted proxy configuration
can change the effective source IP. Valid joins still obey capacity, membership,
version, and code-use checks under the party lock.

Existing matchmaking depth, age, latency, and failure metrics count players.
Monitor claim expiry, short commits, and `matchmaker_gc` cleanup errors. For
party-specific checks, count queued rows in `parties`, overdue rows in
`party_members`, and expired rows in `matchmaking_resolutions`. Avoid player,
party, or code IDs as metric labels.

## Local validation without Docker

The matchmaking and HTTP API test fixtures accept
`MATCHMAKER_TEST_DATABASE_URL` and `HTTPAPI_TEST_DATABASE_URL`. Use dedicated
PostgreSQL 17 databases named `ggscale_matchmaker_template` and
`ggscale_httpapi_template`, owned by the test login `ggscale`, with permission
to create databases and roles. The fixtures apply migrations and clone test
databases. Never point these variables at an application database. Omit them
to use the normal Testcontainers fixtures in Linux CI.
