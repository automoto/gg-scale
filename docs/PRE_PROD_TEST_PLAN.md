# ggscale Pre-Production Integration Test Plan

**Audience:** a coding agent executing tests against ggscale via the HTTP API (`curl`/HTTP client) **and** via the browser using computer use (Claude + Chrome).
**Goal:** catch correctness, authorization, and isolation bugs before the production release.
**Priority order:** **(1) tenant isolation, (2) privilege escalation, (3) player features, (4) new flows (tenant signup, player linking, invites), (5) everything else.**

> This plan supersedes `docs/archive/PRE_PROD_TEST_PLAN.md` (and its two retest notes). It is refreshed for the current **schema baseline** (`0001_baseline` ... `0014_realtime_connection_grants`) and adds coverage for the three areas that landed since the last pass: **admin-initiated player linking**, **invite rate-limit overrides**, and the **tenant + player sign-up pages**. The quota, tier, storage-metering, change-request, regional realtime-cap, platform-admin RBAC, and migration-command changes on `ws-connection-enforcement` have their own focused follow-up in [section 22](#22-branch-follow-up-integration-plan).
>
> **How to read a case:** each area has a **Coverage index** (checkbox list of test IDs) then **Detailed cases** with concrete requests / UI steps, expected results, and a pass/fail line. Work top-to-bottom, record results in [§20 Results log](#20-results-log), and follow [§19 Triage rules](#19-severity--triage) — some failures are *stop-the-release* and must be reported immediately.

---

## 0. Run order

Run the phases in order. Guiding rules:

1. **Build state before you read it; read/deny before you destroy.** Anything that permanently removes or locks a fixture (revoke key, ban, disable, delete, lockout) runs last.
2. **Denied-mutation tests are safe to run early.** Most privilege-escalation and isolation cases *expect* a `403`/`404`, so a passing case changes no state. Only a *failing* (buggy) case mutates — exactly what you want surfaced.
3. **Dedicate throwaway fixtures to destructive cases** (table in §4.4) so happy-path fixtures survive the whole run. Never point a destructive case at A1/A2, `{{A_PUB}}`, `{{A_SEC}}`, or a leaderboard other tests read.
4. **Reset between full reruns** with `make clean && make up && make seed` (Local only). Production is never reset — run only the §17 subset there.

### Phase sequence

| Phase | What | Test IDs | Why here |
|-------|------|----------|----------|
| **P0 — Provision** | `make up`, seed, health; create §4 fixtures (dashboard users via CP‑07 team invites; all API keys; players A1/A2/anon/B1 + throwaways; a leaderboard per tenant; baseline storage `save1` in **both** A1 and B1) | §2, §4, CP‑01, CP‑04..07 | Everything downstream needs these |
| **P1 — Smoke** | Health, spec, login pages | SMOKE‑01..05 | Fast fail if the build is broken |
| **P2 — Player features (build state)** | Auth happy paths, profile, friends up to accepted, sessions + game invites (needs A1↔A2 friends), matchmaking, leaderboard reads + submit, storage, presence/WS. Then reversible friend ops (block/delete) | AUTH‑*, PROF‑*, FRND‑*, SESS‑*, MM‑*, LB‑*, STOR‑*, PRES/WS | Establishes the data that isolation/privesc will probe |
| **P3 — New flows** | Tenant self-signup, player linking, invite rate limits (non-destructive parts) | SIGNUP‑*, LINK‑*, INV‑* | New surfaces; run after fixtures exist |
| **P4 — Browser CRUD** | Remaining control-panel + platform-admin + player-UI happy paths and 2FA | CP‑02/03/08/09/10/11/12/13, PADMIN‑01..05, PUI‑01..08 | Inspect/confirm UI (PADMIN toggles on throwaways) |
| **P5 — Privilege escalation (non-destructive)** | Dashboard RBAC + key/session escalation that only *attempt* forbidden actions | PRIV‑D*, PRIV‑K01/K02, PRIV‑P01/P02/P05/P06/P07 | Expect denials → no state change on a pass |
| **P6 — Tenant isolation** | Cross-tenant reads + denied cross-tenant writes; RLS sanity | ISO‑01..10 | Needs A+B fixtures intact — before P8 destroys them |
| **P7 — Regression re-verify** | Re-check every prior confirmed finding so it can't silently regress | REG‑01..08 | Cheap, high-value; run before destructive |
| **P8 — Edge/validation (non-destructive)** | Validation, payloads, injection/escaping, pagination, methods, idempotency, TTLs | EDGE‑02..11 | Broad sweep; leave lockout for P9 |
| **P9 — Destructive (run last, each on a throwaway)** | Ban/disable, key/grant revocation, delete, lockout | PRIV‑P03/P04, PRIV‑K03/K04, CP‑06(revoke)/08(ban)/09(delete), EDGE‑01(lockout) | These consume/lock fixtures permanently; capture pre-state (a valid token *before* disabling) first |

> **After P9** the environment is dirty (locked/banned/deleted fixtures). Re-run `make clean && make up && make seed` before another full pass. If time-boxed, run P0→P8 for a clean signal and P9 once at the very end.
>
> Run the branch-focused section 22 against a separate fresh Local reset. Its quota-boundary fixtures deliberately alter tenant counts, storage counters, migration state, SMTP availability, and realtime limits; do not mix them into the general P0-P9 run.

---

## 1. Environments

| Env | Base URL | Use for | Destructive |
|-----|----------|---------|-------------|
| **Local** | `http://localhost:8080/v1` | Full suite, incl. destructive privesc/isolation | **Allowed** — free to create/ban/disable/delete and re-seed |
| **Production** | `https://{{PROD_HOST}}/v1` | **Read-only smoke** only (§17) — confirm prod-config differences (Secure cookies, HTTPS, real SMTP, `ENV=production` CORS validation) | **Never** — no writes, no ban/delete/disable, no seed |

Run the **full** plan on Local. On Production run **only** cases tagged **`[prod-smoke]`**, all read-only.

> ⚠️ **Production smoke is currently BLOCKED on config the plan does not have:** the real `{{PROD_HOST}}`, and the expected CORS origin allowlist (allowed + disallowed origins) to assert against. Fill these in (§17) before running that subset. Do **not** guess a host.

### Variables (fill in as you create fixtures, §4). Use verbatim in requests.

```
{{BASE}}            e.g. http://localhost:8080/v1
{{MAILPIT}}         http://localhost:8025               (email catcher UI; JSON API GET /api/v1/messages)
{{CP}}              {{BASE}}/control-panel              (control-panel UI base)
{{PLAYERS}}         {{BASE}}/players                    (player web UI base)
{{PROD_HOST}}       <REQUIRED for §17; unset today>

# API keys (Tenant A unless noted) — create via CP so Casbin g-rows are written (see §3)
{{A_PUB}}           Tenant A publishable key, project-pinned to P1   (anonymous/project auth requires a pin)
{{A_SEC}}           Tenant A secret key                              (server calls: score submit, session verify)
{{A_PUB_P2}}        Tenant A publishable key pinned to project P2    (cross-project replay tests)
{{A_MM}}            Tenant A publishable key WITH matchmaker scope
{{B_PUB}}           Tenant B publishable key, pinned to B/P1
{{B_SEC}}           Tenant B secret key

# Player session tokens (JWT access_token from /v1/auth/*, sent in X-Session-Token; ~15 min TTL)
{{A1_SESSION}} {{A2_SESSION}} {{ANON_SESSION}} {{B1_SESSION}}

# IDs
{{A_TENANT}} {{A_P1}} {{A_P2}} {{B_TENANT}} {{B_P1}}
{{A1_PLAYER_ID}} {{A2_PLAYER_ID}} {{B1_PLAYER_ID}}
{{A_LB_ID}}         a leaderboard id in Tenant A / P1
{{B_LB_ID}}         a leaderboard id in Tenant B
```

### Current local run fixture map (2026-07-14)

Local stack was reseeded with `make seed` on 2026-07-14. Concrete fixture IDs used by the results log below:

- Tenant A = Nebula Interactive (`tenant_id=5`), P1 = Starfall Arena (`project_id=13`), P2 = Nebula Racer (`project_id=17`)
- Tenant B = Ironclad Studios (`tenant_id=6`), P1 = Tank Battalion (`project_id=14`)
- A1 = `player_id=456`, `shawn.santos.72653@fastmail.com`; A2 = `player_id=459`, `kai.silva.18217@icloud.com`; B1 = `player_id=574`, `ellis.kowalski.614@gmail.com`
- A link target candidates: unverified/unlinked A/P1 `player_id` values 457, 458, 460, 461, 462
- A leaderboard = 31; B leaderboard = 39
- Fresh CP-created API key IDs: A_PUB=33, A_SEC=34, A_PUB_P2=35, A_MM=36, B_PUB=37, B_SEC=38, KEY_throwaway=39 (revoked), ADMIN_PUB_ALLOWED=40
- A-OWNER and B-OWNER required first-login email verification via Mailpit after seed. This is realistic and expected.
- Browser connector status: in-app Browser unavailable in this run (`agent.browsers.list()` returned `[]`). Local headless Playwright is available when Chromium is launched outside the sandbox; screenshot-backed browser tests are being run with evidence under `/private/tmp/ggscale-preprod-screenshots`.

---

## 2. Setup & preconditions (Local)

```bash
# 1. Bring up server + Postgres + Mailpit. Migrations (0001..0012) run automatically before the server starts.
make up

# 1a. Before browser fixtures, send a uniquely addressed message over SMTP.
# curl fails if the server does not provide a usable SMTP banner/protocol.
MAIL_SMOKE="ggscale-smoke-$(date +%s)@example.test"
printf 'From: ggscale-smoke@example.test\r\nTo: %s\r\nSubject: ggscale mail smoke\r\n\r\nready\r\n' "$MAIL_SMOKE" \
  | curl -fsS smtp://127.0.0.1:1025 \
      --mail-from ggscale-smoke@example.test --mail-rcpt "$MAIL_SMOKE" -T -

# Require HTTP readiness and read the unique message back through the API.
curl -fsS http://127.0.0.1:8025/readyz
curl -fsS --get --data-urlencode "query=to:$MAIL_SMOKE" \
  http://127.0.0.1:8025/api/v1/search | rg -F "$MAIL_SMOKE"

# 2. Health check — expect {"status":"ok",...} and header X-API-Version: v1
curl -i http://localhost:8080/v1/healthz

# 3. Seed demo tenants/projects/players/keys/leaderboards/etc. (-force truncates demo data then reseeds)
make seed          # == go run ./scripts/ggscale-seed -force

# 4. Read the control-panel bootstrap token (only if testing CP-01 first-run on an empty user table)
cat ./data/bootstrap.token          # also printed in `make logs` at startup

# 5. DB shell for inspecting state / setting a fixture directly (read-mostly; see the hand-insert warning in §3)
make psql

# Full reset between runs (destroys the volume — required if the schema changed):
make clean && make up && make seed
```

**Services** (`docker-compose.yml`): Postgres 17 (`ggscale`/`ggscale`, `127.0.0.1:5432`), auto-migrator, `ggscale-server` on `:8080` (`ENV=dev` → wildcard CORS + insecure cookies for local), Mailpit (SMTP `:1025`, UI `:8025`).

**Seeded credentials** (`scripts/ggscale-seed/main.go`):
- Platform admin: `admin@demo.ggscale` / `Password123!`
- Tenant owners: `owner-1@demo.ggscale`, `owner-2@demo.ggscale`, … / `Password123!`
- Players: shared password `PlayerPass123!`; ~2/3 have emails and are **pre-verified** (`email_verified_at` set), ~1/3 are external-id-only (anonymous).
- The seed creates a bootstrap (secret) key + a publishable key per project but **does not print plaintext** (keys are hashed). **For any key you need the plaintext of, create it fresh via the control panel** (§4.2).

**Seed shape:** 4 tenants (`Nebula Interactive`, `Ironclad Studios`, `PixelForge Games`, `Voidwalkers LLC`; tiers free/payg/premium/payg), 2–3 projects each, 20–54 players/project, plus leaderboards+scores, friends, storage, fleets, allocations, matchmaking tickets, usage samples, audit logs.

> **Map the seed to A/B:** **Tenant A = `Nebula Interactive`** (owner `owner-1@demo.ggscale`), **Tenant B = `Ironclad Studios`** (owner `owner-2@demo.ggscale`). Use each tenant's first project as **P1**; use a second Tenant A project as **P2**. Read the concrete IDs from the CP after seeding (they are sequential from a fresh baseline but do not hard-code them — the seed randomizes counts).

**Email:** every signup / email-change / invite / verification in Local sends to **Mailpit**. Read codes/links at `{{MAILPIT}}` (browser) or `GET {{MAILPIT}}/api/v1/messages` (JSON). Seeded email players are already verified.

---

## 3. Conventions

**Auth headers** (exact names):
- API key: `Authorization: Bearer <key>` (scheme case-insensitive).
- Player session: `X-Session-Token: <jwt>` (the `access_token` from `/v1/auth/*`, ~15 min TTL).
- Control-panel session: cookie `ggscale_control_panel_session` (12 h TTL, slides after 1 h idle) set on login; CSRF via header `X-CSRF-Token` or form field `_csrf` (session-bound).
- Player-site session: cookie `ggscale_player_session` (30 d TTL); CSRF via cookie `ggscale_csrf` + form `_csrf`/header `X-CSRF-Token` (double-submit).
- Unauthenticated CP magic-link/public pages (invite accept, tenant signup): double-submit CSRF cookie, no session.

**Getting a player session (API):**
```http
POST {{BASE}}/auth/anonymous            Authorization: Bearer {{A_PUB}}
# → { access_token, refresh_token, player_id, external_id, expires_at }.  Use access_token as {{A1_SESSION}} etc.
```
or `POST {{BASE}}/auth/login` `{"email","password"}` with the same `Authorization: Bearer {{A_PUB}}`. **Anonymous/project-scoped auth requires a project-pinned publishable key** — a tenant-wide key returns `400 api key has no project pin`.

**Token/expiry facts (from source):** access token `15m`; refresh `30d` (rotated on `/auth/refresh`, old token invalidated); email verify code `15m` TTL, **5 attempts per code / 20 lifetime, then 24h lockout (429)**; signup duplicate-notice cooldown `15m`; game invite short TTL (~5m); **player link/invite magic-link TTL 3 days**; **team + tenant-signup invite expiry 7 days**.

**Test-case block format:**
```
### <ID> — <title>   [tags]
Pre: <preconditions>
Do:  <request or UI steps>
Expect: <status / body / observable assertion>
[ ] pass  [ ] fail   notes: __________
```
Tags: `[api]` `[browser]` `[prod-smoke]` `[critical]` (a failure here blocks release).

**Deny outcomes:** `401` = not authenticated, `403` = authenticated but forbidden, `404` = hidden/absent (an acceptable deny). A `200` returning another tenant's data — or performing a forbidden mutation — is **always a critical fail**. Prefer verifying the *effect* (was the row actually written/changed?) via a follow-up read or `make psql`, not just the status code.

> ⚠️ **Do NOT hand-insert keys/memberships/casbin rows in Postgres.** The control panel writes Casbin grouping (`g`) rows *in Go, in the same transaction* as the membership/key insert. A raw `INSERT` (or a direct seed of these) omits those rows and produces **false `403`s** that look like product bugs but are harness artifacts. Create API keys via **CP → API keys → New** and members via **CP → Team → Invite**. This was the single biggest source of noise in the last pass.

---

## 4. Required test accounts & fixtures

### 4.1 Dashboard (control-panel) users

| Handle | Email | Role → capability | Tenant | Create via |
|--------|-------|-------------------|--------|------------|
| **PA** | `admin@demo.ggscale` | platform admin — `/admin/*`, rate-limit ceilings, tenant-signup approvals | global | seeded |
| **A‑OWNER** | `owner-1@demo.ggscale` | `owner` — full tenant; only role that manages **team**, **secret keys**, **member role grants** | A | seeded |
| **A‑ADMIN** | `admin-a@demo.ggscale` | `admin` — manage project/players/**publishable** keys/leaderboards; **no** team, **no** secret keys, **no** fleet-role grants | A | A‑OWNER invites as "Tenant admin" |
| **A‑MEMBER** | `member-a@demo.ggscale` | `member` — **read-only** | A | A‑OWNER invites as "Tenant member" |
| **A‑FLEET** | `fleet-a@demo.ggscale` | `member` + granted `fleet_operator` | A | invite as member, then A‑OWNER grants fleet_operator |
| **B‑OWNER** | `owner-2@demo.ggscale` | `owner` | B | seeded |

> The team-invite dropdown exposes only **Tenant admin** and **Tenant member**. `owner` is granted only by creating a tenant. If you find a UI path to assign `developer`/`support`/`security_admin`/`platform_admin` to a normal tenant member, that itself is a **finding**.

### 4.2 API keys — create via **CP → {{A_TENANT}} → API keys → New** (plaintext shown once, capture it)

| Handle | Type | Scope | Project pin | Why |
|--------|------|-------|-------------|-----|
| **{{A_PUB}}** | publishable | default | **P1** | player-facing calls, anonymous/signup auth |
| **{{A_SEC}}** | secret | default | P1 or tenant-wide | server calls: score submit, session verify |
| **{{A_PUB_P2}}** | publishable | default | **P2** | cross-project replay/confinement |
| **{{A_MM}}** | publishable | **+ matchmaker** | P1 | matchmaking (matchmaker scope is default-on for new keys — **record whether a fresh key already has it**, MM‑00) |
| **{{B_PUB}}** | publishable | default | B/P1 | Tenant B isolation |
| **{{B_SEC}}** | secret | default | B | cross-tenant verify |

Secret-key creation is **owner-only** — create the secret keys as A‑OWNER. To create the noscope MM control key, create a publishable key then **revoke** its matchmaker scope via the key's **Features** action.

### 4.3 Players (identities)

| Handle | Kind | Tenant/Project | Linked global account? | Why |
|--------|------|----------------|------------------------|-----|
| **A1** | email player, verified | A / P1 | yes | primary player flows, friends |
| **A2** | email player, verified | A / P1 | yes | friend/session counterpart |
| **A‑ANON** | anonymous session | A / P1 | no | anonymous boundary (friends → 403) |
| **B1** | email player, verified | B / P1 | yes | cross-tenant counterpart |

Use two seeded, pre-verified email players from Tenant A / P1 as A1 and A2 (read their emails from **CP → Players**; password `PlayerPass123!`). Friends operate on **global `player_accounts`** — if a friend call returns `403 "link a gg-scale account"`, the identity isn't linked; use a linked seeded player or link one via the player site (`{{PLAYERS}}/account/signup` → join project).

### 4.4 Throwaway fixtures to create in P0 (so P9 never touches real ones)

| Fixture | Used by | Note |
|---------|---------|------|
| `A‑DISABLED` player | PRIV‑P03, CP‑08 disable | capture a valid, unexpired session token **before** disabling |
| `A‑BANNED` player_account | PRIV‑P04, CP‑08 ban | tenant-level ban; **requires a linked account**; verify across all A projects |
| `KEY‑throwaway` (publishable) | PRIV‑K04, CP‑06 revoke | so `{{A_PUB}}` survives the run |
| `LB‑throwaway` leaderboard | CP‑09 delete | so `{{A_LB_ID}}` survives |
| `lockme@demo.ggscale` account | EDGE‑01 lockout | gets rate-limited; never reuse |
| `A‑LINK-TARGET` unverified player | LINK‑* | a project player with `email_verified_at IS NULL` (an anonymous/external-only seeded player, or create one) |

---

## 5. Smoke & health

**Coverage index**
- [ ] SMOKE‑01 healthz `[api][prod-smoke]`
- [ ] SMOKE‑02 control-panel login renders `[browser][prod-smoke]`
- [ ] SMOKE‑03 player site login renders `[browser][prod-smoke]`
- [ ] SMOKE‑04 unknown route 404 `[api][prod-smoke]`
- [ ] SMOKE‑05 OpenAPI spec is current `[api]` (Local only)

**SMOKE‑01** `GET {{BASE}}/healthz` → `200`, `{"status":"ok",...}`, header `X-API-Version: v1`.
**SMOKE‑02** navigate `{{CP}}/login` → email+password form; Pico CSS + assets load; 0 console errors.
**SMOKE‑03** navigate `{{PLAYERS}}/account/login` → login form + signup link; 0 console errors.
**SMOKE‑04** `GET {{BASE}}/does-not-exist` → `404`, no stack trace / internal detail.
**SMOKE‑05** `go run ./cmd/openapi-dump /tmp/new.yaml && diff openapi.yaml /tmp/new.yaml` → no diff.
[ ] pass [ ] fail  notes: ______

---

## 6. Auth & sessions (API)

**Coverage index**
- [ ] AUTH‑01 anonymous · 02 signup→verify→login · 03 login before verify denied · 04 wrong password · 05 duplicate signup · 06 over-long password · 07 bad/expired verify code · 08/09 refresh rotation+reuse `[critical]` · 10 logout · 11 bad api keys `[prod-smoke]` · 12 player route w/o session · 13 custom-token · 14 verify attempt budget + lockout

**AUTH‑01** `POST /auth/anonymous` + `{{A_PUB}}` → `200`, `access_token`/`refresh_token`/`player_id`/`external_id`. Save `{{ANON_SESSION}}`.
**AUTH‑02** signup `{"email":"newp@example.com","password":"CorrectHorse9!"}` + `{{A_PUB}}` → `202`; read 6-digit code from Mailpit; `POST /auth/verify` → `200`; `POST /auth/login` → `200` with `access_token`.
**AUTH‑03** `[critical]` sign up but do NOT verify, then `POST /auth/login` → **`403 "email not verified"`** (this was a confirmed bug, now fixed — see REG‑01).
**AUTH‑04** login A1 with bad password → `401`, generic message (no user-exists oracle).
**AUTH‑05** signup same email twice → second does not create a second account; response/timing indistinguishable from a fresh email (Mailpit gets an "existing account" notice; note the `15m` notice cooldown).
**AUTH‑06** signup with a 100-byte ASCII password → clear `4xx` (bcrypt max 72), not `500`, not silent truncation.
**AUTH‑07** verify with `000000` → `4xx`; separately, verify with a real code whose `email_verification_expires_at` is in the past → `4xx`, no state change.
**AUTH‑08/09** `[critical]` `POST /auth/refresh` with A1 refresh → new pair (`200`); reuse the **old** refresh → `401`. Reuse succeeding is a **critical** session-fixation risk.
**AUTH‑10** `POST /auth/logout` with A1 refresh → `204`; then refresh with it → `401`.
**AUTH‑11** `[prod-smoke]` `/auth/anonymous` with (a) no `Authorization` → `401`; (b) `Bearer garbage` → `401`; (c) a revoked key → `403`. No tenant info leaked.
**AUTH‑12** `GET /profile` with `{{A_PUB}}` but no `X-Session-Token` → `401`.
**AUTH‑13** `POST /auth/custom-token` with a tenant-signed JWT (`aud=ggscale-custom-token`, valid `exp`, `external_id`) → `200`; same claims signed with the wrong key → `401`.
**AUTH‑14** exhaust the verify attempt budget: submit wrong codes for one unverified account — after 5 wrong attempts on a code and/or 20 lifetime, expect `429` and a 24h lockout; a correct code during lockout is still refused.
[ ] pass [ ] fail  notes: ______

---

## 7. Player features (API)

Use A1/A2 sessions unless noted. **Every player route needs both `Authorization: Bearer <key>` and `X-Session-Token`.**

**Coverage index**
- [ ] PROF‑01 get · 02 patch xuid · 03 patch email (verify round-trip) · 04 validation
- [ ] FRND‑01 request · 02 accept · 03 list+presence · 04 reject · 05 block · 06 unblock · 07 delete · 08 anonymous denied `[critical]` · 09 friend remote-addrs · 10 non-friend remote-addrs denied `[critical]`
- [ ] SESS‑01 create · 02 resolve join_code · 03 join · 04 heartbeat · 05 leave/host-end · 06 game-invite friend · 07 invite non-friend denied · 08 invite TTL · 09 max_players (0=default, negative→400, huge→400)
- [ ] MM‑00 default-scope probe · 01 create ticket · 02 poll→matched · 03 cancel · 04 missing scope 403 `[critical]` · 05 property limits
- [ ] LB‑01 top · 02 around-me · 03 submit (secret) `[critical]` · 04 submit with publishable denied `[critical]` · 05 rank updates
- [ ] STOR‑01 put · 02 get · 03 list prefix · 04 If-Match concurrency `[critical]` · 05 invalid JSON · 06 oversized→413 · 07 delete
- [ ] PRES‑01 update · 02 appears in friend list · WS‑01 connect · WS‑02 per-player cap

**Profile** — `GET /profile` → own profile only. `PATCH /profile {"xuid":"gamer_01"}` → `204`, re-GET shows it. `PATCH /profile {"email":...}` → `202` (verification round-trip; email not changed until verified). Validation: 65-char xuid, control-char xuid → `4xx`; empty patch → `4xx`/no-op.

**Friends** — request/accept between A1↔A2; both see each other `accepted` with a presence field. Reject, block (blocked party can't re-request/invite), unblock, delete (removes edge for both). **FRND‑08 `[critical]`:** anonymous `POST /friends/{{A1_PLAYER_ID}}/request` → `403`. **FRND‑09/10 `[critical]`:** accepted-friend `GET /friends/{id}/remote-addrs` → `200` with addrs; a **stranger** or a **Tenant B** player id → deny (`403`/`404`), never leaking another player's network addresses. These touch the un-RLS'd global `player_accounts` — probe several ids.

**Game sessions & invites** — create (`max_players`, `public_addr`) → `201` with `session_id`/`join_code`/`state`; resolve by `?joinCode=`; join as A2; heartbeat; host `DELETE` ends, non-host leaves. `POST /invite {"to_email","session_id"}` for friend A2 → `201`, visible in `GET /invite`; non-friend → deny; invite past its short TTL no longer resolves. `max_players`: `0` = default (by design), **negative → `400`**, huge → `400`.

**Matchmaking** — **MM‑00:** with a fresh publishable key (no scope edits), `POST /matchmaker/tickets {"mode":"match_only","game_mode":"ffa","min_count":1,"max_count":2}` — record whether allowed (scope default-on) or `403`. **MM‑01/02:** with `{{A_MM}}`, create → `201 queued`; queue two compatible tickets → both progress to `matched` with a shared `match_id` and a `users` roster. **MM‑03:** cancel a queued ticket → `204`. **MM‑04 `[critical]`:** with `{{A_PUB}}` (no matchmaker scope) → `403`. **MM‑05:** `string_properties` > 16 entries or a value > 128 bytes, or reserved key (`region`) → `400`. Confirm server logs show **0** `permission denied for table matchmaking_tickets` during this (the worker must drain cleanly).

**Leaderboards** — top / around-me reads → `200`. **LB‑03 `[critical]`:** submit with **secret** `{{A_SEC}}` + `X-Session-Token` → `201`. **LB‑04 `[critical]`:** same submit with **publishable** `{{A_PUB}}` → `403` (server-authoritative writes need a secret key). Submit a higher score → top/rank reflects it.

**Storage** — `PUT /storage/objects/save1` (valid JSON body) → `200 version:1`; GET returns value+version; list `?key_prefix=save` → caller's objects only. **STOR‑04 `[critical]`:** `If-Match: 1` → `200` (version→2); repeat `If-Match: 1` → `412`. Invalid JSON → `4xx`. **STOR‑06:** a value **over the configured storage cap** → `413`, nothing stored (current local default from `STORAGE_MAX_VALUE_BYTES` / code fallback is **1 MiB**). Delete → `204`, subsequent GET → `404`.

**Presence & realtime** — `PUT /presence {"status":"online"}` → `200`; friend's `/friends` reflects it. `GET /ws` with `Authorization` + `X-Session-Token` → `101`. Open more than `REALTIME_MAX_PER_PLAYER` (default 4) connections for one player → excess rejected (`503 "too many connections for this user"`); closing sockets frees slots. Probe `REALTIME_MAX_PER_TENANT` if configured (default 0 = disabled).
[ ] pass [ ] fail  notes: ______

---

## 8. Control panel — tenant/project admin (browser)

Use computer use; log in at `{{CP}}/login`. Record a `login_flow.gif` for auth + a representative CRUD flow.

**Coverage index**
- [ ] CP‑01 first-run bootstrap `[browser]` · 02 login+logout `[browser][prod-smoke]` · 03 2FA lifecycle · 04 create tenant · 05 create project · 06 API key CRUD (create/label/scope/revoke) · 07 team invite/accept/role/remove/revoke · 08 players list/detail/disable/ban/invite/**link** · 09 leaderboard CRUD · 10 rate limits · 11 settings/public-joining · 12 CSRF/session negatives

**CP‑01** (fresh DB only) `{{CP}}/setup` → paste `./data/bootstrap.token` → set admin email + password (≥12 chars) → verify via Mailpit. Admin created; token single-use (re-submit fails, `410`).
**CP‑02** `[prod-smoke]` login as A‑OWNER; unverified seeds route to `/verify` (Mailpit code); home lists tenants; logout clears cookie; protected URL after logout → `/login`.
**CP‑03** Account → enable 2FA (scan/enter secret, confirm 6-digit) → logout/login with challenge + "trust this device" → regenerate backup codes → a backup code works **once** → disable (needs password + code). Disabling revokes other sessions/trusted devices; backup code single-use.
**CP‑04** New tenant → tenant/project/key label → submit → tenant + first project + API key created, key shown once; creator becomes `owner`.
**CP‑05** Tenant A → Projects → New → duplicate name within tenant → `409`.
**CP‑06** create a publishable and a secret key; edit a label; grant then revoke a scope (Features action); revoke a key → revoked key immediately fails an API call (cross-check with `curl`).
**CP‑07** as A‑OWNER, invite `admin-a@…` (Tenant admin) and `member-a@…` (Tenant member) via the `{{CP}}/invite/accept?code=` Mailpit magic links; change a member's role (à-la-carte grant); remove a member; revoke a pending invite. Removed member loses access.
**CP‑08** Project → Players → open a player → Disable toggle, Ban toggle (ban **requires a linked account**); Invite a player by email; **Link** an unverified player (see §11). Disable/ban enforced on API (cross-check PRIV‑P03/P04). Emails land in Mailpit.
**CP‑09** create a leaderboard (name + sort order), edit, delete → CRUD persists.
**CP‑10** view rate-limits page; as **tenant owner** update a project invite quota (persists); as **platform admin** update the tenant API ceiling and the per-recipient invite override (§12). A non-platform-admin sees "Only platform admins can change the API ceiling".
**CP‑11** edit tenant + project settings; toggle public-joining at tenant and project level → persistence + joinability changes.
**CP‑12** (a) replay a mutation POST (e.g. create key) with `_csrf`/`X-CSRF-Token` removed → `403`; (b) submit with a stale/other-user CSRF token → `403`; confirm no state changed.
**CP‑13** as any tenant user, Account page: change password (verify current password required, new ≥12 chars) → success; wrong current password → deny.
[ ] pass [ ] fail  notes: ______

---

## 8A. Platform admin console (browser)  `[NEW]`

Positive flows for the platform-admin-only `/admin/*` console (log in as **PA** = `admin@demo.ggscale`). These complement the *denial* checks in PRIV‑D09/SIGNUP‑10 (which prove non-PAs are blocked). Run the destructive toggles on **throwaway** fixtures. Tenant-signup review pages are covered in §10.

**Coverage index**
- [ ] PADMIN‑01 control-panel users: list renders; disable a throwaway user blocks their login; enable restores `[browser]`
- [ ] PADMIN‑02 global player accounts: list + detail (linked players across tenants + ban history) render; disable blocks login; enable restores `[browser]`
- [ ] PADMIN‑03 platform team: invite a platform-admin candidate, accept magic link, confirm `is_platform_admin` `[browser]`
- [ ] PADMIN‑04 `/admin/settings` and `/admin/plugins` render (plugins page shows "no backend" if fleet disabled) `[browser]`
- [ ] PADMIN‑05 cross-tenant management: PA (no membership) can open any tenant's CP pages `[browser][critical]`

**PADMIN‑01** `{{CP}}/admin/users` → list shows email/last_login/created_at. Disable a **throwaway** control-panel user → that user's next login is blocked; enable → login works again. (Active sessions may persist by design — note the behavior.)
**PADMIN‑02** `{{CP}}/admin/player-accounts` → list of linked global accounts; open one → detail shows linked project players across tenants + ban history. Disable a **throwaway** global account → its player login is blocked (account data persists); enable → restored.
**PADMIN‑03** `{{CP}}/admin/team/invite` → invite a throwaway platform-admin candidate email; accept the Mailpit magic link (`{{CP}}/invite/accept?code=`); confirm the new user is a platform admin (can reach `/admin/*`, holds no tenant membership). Note: platform-admin invites are **not throttled** — trusted path. Clean up (disable) afterward.
**PADMIN‑04** `{{CP}}/admin/settings` and `{{CP}}/admin/plugins` render without error; plugins shows plugin snapshot (name/version/PID/health) or a "no backend configured" state when fleet is disabled locally.
**PADMIN‑05 `[critical]`** as PA with **no membership** in Tenant B, open `{{CP}}/tenants/{{B_TENANT}}/rate-limits`, `/settings`, `/projects`, `/team` → all `200` (platform admins manage any tenant's CP by design). This is the *inverse* of PRIV‑D10 (a tenant owner must NOT reach another tenant) — confirm both hold.
[ ] pass [ ] fail  notes: ______

---

## 9. Player web UI (browser)

Log in at `{{PLAYERS}}/account/login`.

**Coverage index**
- [ ] PUI‑01 signup→verify→home (new signup page) · 02 login+logout `[browser][prod-smoke]` · 03 2FA lifecycle · 04 friends (add/accept/unfriend/block/unblock) · 05 join project · 06 remote addresses (IP/DNS/Iroh, 4 slots) · 07 project invite acceptance (magic link) · 08 output escaping / stored XSS `[critical]`

**PUI‑01** `{{PLAYERS}}/account/signup` with email + display name + password (≥8) → verify via Mailpit code → account home shows display name/email. Resend code respects the 1-min cooldown.
**PUI‑02** `[prod-smoke]` login as A1 → account home; logout → protected page redirects to login.
**PUI‑03** enable/confirm 2FA, logout/login with challenge + trust-device, regenerate backup codes, disable (password + code). Disabling revokes other sessions.
**PUI‑04** as A1 add A2 by email/display name; as A2 accept; verify Friends/Incoming/Sent/Blocked sections; unfriend; block then unblock. (Friend search must be enumeration-resistant — a non-existent handle returns the same generic result.)
**PUI‑05** on account home, join a project by ID → lists under linked games. Use a public project (a project made invite-only in CP‑11 must reject open joins).
**PUI‑06** add IP (LAN vs Public scope labels), DNS, and Iroh (64-hex) addresses across the 4 slots; edit and save.
**PUI‑07** open the CP‑08 player invite magic link `{{PLAYERS}}/p/{projectID}/invite/accept?code=…`; for a new account set a password (≥8) → account created/linked to the project.
**PUI‑08 `[critical]`** set display name and a remote-address to `<img src=x onerror=alert(1)>` and `"><script>alert(1)</script>`; view account home, friends list, **and** the CP Players view. All render as inert text; no dialog fires. **Read the DOM to confirm escaping — do not rely on an alert firing (a dialog would block the browser session).**
[ ] pass [ ] fail  notes: ______

---

## 10. Tenant self-signup (request → approve → accept)  `[NEW]`

Two-stage public flow backed by `tenant_signup_requests` (migration 0004). Public request → platform-admin approval → invited user sets password → tenant+owner created. Public pages use double-submit CSRF, no session.

**Coverage index**
- [ ] SIGNUP‑01 public request form renders `[browser]`
- [ ] SIGNUP‑02 submit request → generic ack (anti-enumeration) `[browser]`
- [ ] SIGNUP‑03 duplicate email / taken tenant name handled silently `[api][critical]`
- [ ] SIGNUP‑04 request validation (name 2–60, no control chars; description ≤2000; studio ≤120) `[api]`
- [ ] SIGNUP‑05 platform-admin review list + enable/disable toggle `[browser]`
- [ ] SIGNUP‑06 approve → magic-link email; accept as NEW user (≥12-char password) → tenant+owner created + auto-login `[browser][critical]`
- [ ] SIGNUP‑07 accept as EXISTING user requires **current password** (magic link alone can't hijack) `[browser][critical]`
- [ ] SIGNUP‑08 deny → denial email; code no longer accepted `[browser]`
- [ ] SIGNUP‑09 public signup gate: when disabled, `/request-access` refuses `[api]`
- [ ] SIGNUP‑10 non-platform-admin cannot reach `/admin/tenant-signups*` `[api][critical]`
- [ ] SIGNUP‑11 accept code expiry / single-use / tampered code `[api][critical]`

**SIGNUP‑01/02** navigate `{{CP}}/request-access` → form (email, tenant_name, project_description, studio_name). Submit valid values → a **generic acknowledgement page** regardless of whether the email is new/known. A `tenant_signup_requests` row is created (verify via `make psql`).
**SIGNUP‑03 `[critical]`** submit a request for an email that already has a request/account, and separately for an **already-taken tenant name**. Expect: the ack response/timing is **indistinguishable** from a fresh request (no enumeration), and no duplicate live request/tenant is created. A distinguishable response is a Medium finding; a created duplicate tenant is critical.
**SIGNUP‑04** submit tenant_name of 1 char, 61 chars, and one containing a control char; description > 2000 chars; studio > 120 chars → `4xx` clear validation, no `500`.
**SIGNUP‑05** as **PA**, `{{CP}}/admin/tenant-signups` → pending list renders with email/tenant/description + current enabled state. Toggle enable/disable (`/admin/tenant-signups/config`) → state + audit persists.
**SIGNUP‑06 `[critical]`** as PA, approve a pending request → approval email with `{{CP}}/request-access/accept?code=…` lands in Mailpit. Open it (new user path): set a password (≥12) → a control_panel_user is created with **owner** role on a new tenant, and the user is **auto-logged-in** and redirected to the tenant. Verify the new user holds owner (can reach Team) and cannot reach `/admin/*`.
**SIGNUP‑07 `[critical]`** approve a request whose email matches an **existing** control-panel user. Open the accept link → the flow must require the **existing account's current password** (a bare magic link must not create/attach a tenant without it). A wrong current password → deny; correct → new tenant created for that account.
**SIGNUP‑08** deny a pending request (optional reason) → denial email sent; re-opening that request's accept code → rejected.
**SIGNUP‑09** with public signup **disabled** (SIGNUP‑05), `POST {{CP}}/request-access` → refused (record exact status/behavior); enabling restores it.
**SIGNUP‑10 `[critical]`** as A‑OWNER (not PA), `GET/POST` each `/admin/tenant-signups`, `/admin/tenant-signups/config`, `/admin/tenant-signups/{id}/approve`, `/deny` → all `403`. Any pending-request data rendered to a non-PA is a **critical** leak (requester emails/tenant names).
**SIGNUP‑11 `[critical]`** accept with (a) an expired code (past its window), (b) an already-used code, (c) a tampered/random code → all deny (`4xx`), no tenant/user created.
[ ] pass [ ] fail  notes: ______

---

## 11. Player linking — admin-initiated email link  `[NEW]`

`CP → Players → {unverified player} → Link` opens a modal (read-only external ID + email field) and emails a magic-link invite carrying the target `project_player_id` (migration 0005). On accept, the proven email + global account **bind onto that existing row** (no new row); the row becomes verified. Routes: `GET/POST {{CP}}/tenants/{t}/projects/{p}/players/{playerID}/link` (admin-only, CSRF, throttled).

**Coverage index**
- [ ] LINK‑01 Link button shows only on **unverified** rows; not on verified `[browser]`
- [ ] LINK‑02 modal shows read-only external ID; prefills any existing unverified email `[browser]`
- [ ] LINK‑03 send link → magic-link email; accept binds email/account onto the **existing** row (no new row) `[api][critical]`
- [ ] LINK‑04 email collision: email owned by another player in the project → `409`, no invite sent `[api][critical]`
- [ ] LINK‑05 resend supersedes the prior open invite (revoke-then-insert) `[api]`
- [ ] LINK‑06 "invite pending" badge appears while an invite is open `[browser]`
- [ ] LINK‑07 accept conflict: target row already linked to another account → `409`, row untouched `[api][critical]`
- [ ] LINK‑08 accept-flow TTL / tampered code deny `[api]`
- [ ] LINK‑09 privesc: A‑MEMBER cannot Link; A‑ADMIN can only within own tenant; no cross-tenant/cross-project link `[api][critical]`

**LINK‑01/02/06** in CP Players, confirm the **Link** action renders only on rows with `email_verified_at IS NULL`; a verified+emailed row has no Link button. Open the modal → external ID read-only, email prefilled if the row already carries an unverified self-signup email. After sending, the row shows an **"invite pending"** badge (instead of "unverified") while the invite is open.
**LINK‑03 `[critical]`** Link `A‑LINK-TARGET` to a fresh email → Mailpit magic link `{{PLAYERS}}/p/{p}/invite/accept?code=…`. Accept it → verify (via `make psql`) that the **same** `project_players.id` now has `email`, `email_verified_at`, and `player_account_id` set — **no second row was created**. The player can now sign in and use account-linked features (friends).
**LINK‑04 `[critical]`** attempt to Link a player to an email **already owned by another player in the same project** → `409` (`errPlayerEmailTaken`), **no invite sent** (Mailpit empty for that address). No identity merge.
**LINK‑05** Link the same player twice (resend) → the prior open invite is revoked and a fresh one inserted (dodges the `(project_id, email)` open-invite unique constraint); only the latest code accepts.
**LINK‑07 `[critical]`** target a row that is **already linked to a different account**, then accept → `409` (`BindPlayerLinkedEmail` account guard), the row is left untouched (no rebind onto another account).
**LINK‑08** accept with an expired (>3 day) or tampered code → deny; no bind.
**LINK‑09 `[critical]`** as **A‑MEMBER**, forge `POST …/players/{id}/link` → `403`. As **A‑ADMIN**, Link within Tenant A works but forging a Link into a **Tenant B** project (`/tenants/{{B_TENANT}}/projects/{{B_P1}}/players/{id}/link`) → `403`/`404`, no invite, no bind.
[ ] pass [ ] fail  notes: ______

---

## 12. Invites & rate limits  `[NEW]`

Three invite surfaces share one throttle (`DefaultInviteLimits`: **InviterPerHour 10, DomainPerDay 100, RecipientBurst 2, RecipientCooldown 10m**): team invites (`domainKey=tenant:{id}`), player invites and player-link invites (`domainKey=project:{id}`). Platform admins override the per-recipient burst/cooldown and the API ceiling; tenant admins set per-project invite quotas (clamped to the compiled defaults).

**Coverage index**
- [ ] INV‑01 recipient burst then cooldown: 2 back-to-back re-invites to one address succeed, 3rd within 10m → `429` `[api]`
- [ ] INV‑02 inviter-per-hour cap (10/hr) → `429` after budget; failed sends refund quota `[api]`
- [ ] INV‑03 domain-per-day cap (100/day) `[api]`
- [ ] INV‑04 PA sets per-recipient override (burst + cooldown); both-zero clears; one-sided zero rejected `[browser][api]`
- [ ] INV‑05 PA-set override actually changes throttle behavior (raise burst → more sends allowed) `[api]`
- [ ] INV‑06 tenant admin sets per-project quotas; cannot exceed compiled defaults (clamped) `[browser][api]`
- [ ] INV‑07 API rate ceiling: PA-only; non-PA denied write `[api][critical]`
- [ ] INV‑08 invite magic links: TTL (team/tenant 7d; player/link 3d), single-use, tampered code deny `[api][critical]`

**INV‑01** re-invite the same recipient email (team or player invite): 1st + 2nd within seconds → accepted; 3rd within 10 min → `429`. After the cooldown, one more is allowed.
**INV‑02** from one inviter, send 10 player invites to distinct addresses in a project within the hour → all accepted; 11th → `429`. Trigger a send **failure** (e.g. invalid recipient) and confirm the quota is **refunded** (the failed attempt doesn't consume a token).
**INV‑03** exceed 100 invites for one `domainKey` in a day → `429` (may be slow; can be validated at the throttle layer or via `make psql` bucket inspection rather than 100 real sends — note the method used).
**INV‑04** as **PA**, `{{CP}}/tenants/{t}/rate-limits` → set per-recipient **burst** + **cooldown_secs** (`/rate-limits/invites/recipient`). Setting **both to zero clears** the override (reverts to defaults). Setting **one to zero** (burst=0 with a cooldown, or vice-versa) → **rejected** (`errIncompleteLimit`). Burst must be a whole number ≥1. The page reverse-derives the displayed cooldown from the stored refill rate.
**INV‑05** raise the recipient burst to e.g. 5 as PA, then re-run INV‑01 for that tenant → 5 back-to-back sends now succeed (override honored by `InviteThrottle.WithOverrides`). Lower it back / clear.
**INV‑06** as **tenant owner/admin**, `/rate-limits/projects/{p}/invites` → set `inviter_per_hour` + `domain_per_day`. Values above the compiled default (10 / 100) are **clamped** (a tenant admin cannot raise its own ceiling above the default). Values persist and apply.
**INV‑07 `[critical]`** `POST /tenants/{t}/rate-limits/api` and `/rate-limits/invites/recipient` as a **non-platform-admin** (A‑OWNER) → `403`. Only PA may change the tenant API ceiling / per-recipient override. (A tenant owner self-raising its API ceiling would be an abuse/DoS-budget bypass.)
**INV‑08 `[critical]`** for each invite type, confirm the magic link: expires (team/tenant-signup 7d, player/link 3d), is **single-use** (accepting twice → second fails), and a tampered/forged code is rejected — no membership/link granted.
[ ] pass [ ] fail  notes: ______

---

## 13. Privilege escalation — dashboard RBAC (priority)

Policy: **owner** = manage tenant + team + secret keys + member-role grants + everything admin can. **admin** = manage project/players/**publishable** keys/leaderboards; **no** team, **no** secret keys, **no** fleet-role grants. **member** = read-only. No tenant role has a `feature_grants` policy; feature grants are platform-admin only.

For each case: log in as the lower role, attempt the action **both** via the UI (is the control even present?) **and** by **forging the underlying POST** (copy a legitimate request from a higher-role session, swap in the lower-role session cookie + that session's CSRF token). **UI hiding is not enforcement — the forged POST result is the real test.**

**Coverage index**
- [ ] PRIV‑D01 member: API keys `[critical]` · D02 member: create project · D03 member: team mutations · D04 member: player disable/ban · D05 member: settings/rate-limits · D06 admin: **secret** key creation `[critical]` · D07 admin: **team management** `[critical]` · D08 admin: **fleet-operator role grant** `[critical]` · D09 non-PA blocked from `/admin/*` · D10 cross-tenant CP access `[critical]` · D11 fleet_operator confinement · D12 ban authorizer mapping

**PRIV‑D01** `[critical]` A‑MEMBER: `POST …/api-keys` (create) and `…/api-keys/{id}/revoke` → denied; nothing created/revoked (verify as owner).
**PRIV‑D02** A‑MEMBER: `POST …/projects` → denied.
**PRIV‑D03** A‑MEMBER: invite / role-change / remove team member → all denied.
**PRIV‑D04** A‑MEMBER: `…/players/{id}/disable` and `/ban` → denied; player unaffected.
**PRIV‑D05** A‑MEMBER: POST tenant settings, project settings, rate-limit endpoints, public-joining toggles → all denied.
**PRIV‑D06** `[critical]` **A‑ADMIN** create an API key with `key_type=secret` (submit the POST even if UI hides it) → **`403`**. Admin holds `api_key:publishable` only; a secret key created here is critical (secret keys submit scores, verify sessions, run fleet heartbeats). *(Confirmed bug last pass — now fixed; this is REG‑02.)*
**PRIV‑D07** `[critical]` **A‑ADMIN** `POST …/team/invite` (and revoke-invite) → **`403`** (only owner holds `team:manage`). *(Confirmed bug last pass — now fixed; REG‑03.)*
**PRIV‑D08** `[critical]` **A‑ADMIN** `POST …/team/members/{uid}/roles` granting `fleet_operator` (and member-remove) → **`403`** (owner-only). *(Confirmed bug last pass — now fixed; REG‑04.)* Then confirm feature-grant toggles are denied for **both** admin and owner, and only **PA** can. A tenant owner/admin self-granting a paid/gated feature is a **critical** billing/entitlement bypass.
**PRIV‑D09** A‑OWNER (not PA): navigate/POST `/admin/users`, `/admin/team`, `/admin/player-accounts`, `/admin/settings`, `/admin/tenant-signups` and their action routes → all `403`.
**PRIV‑D10** `[critical]` A‑OWNER: request Tenant B URLs directly (`/tenants/{{B_TENANT}}/projects`, `/api-keys`, `/team`, a B project's players) → denied; **any B data rendered is a critical isolation break**.
**PRIV‑D11** A‑FLEET (member + fleet_operator): can reach fleet/allocation management but **not** api_key/team/project-config mutations. *(Fleet is often disabled locally — if `FEATURE_FLEET_ENABLED=false`, record the surfaces as unmounted and confine-test what is reachable.)*
**PRIV‑D12** empirically map the player disable/ban authorizer (no explicit `ban` policy — handler checks project/players manage): confirm member denied, admin+owner allowed; record the true rule.
[ ] pass [ ] fail  notes: ______

---

## 14. Privilege escalation — API keys & player sessions (priority)

**Coverage index**
- [ ] PRIV‑K01 publishable rejected on secret-only routes `[critical]` · K02 missing scope `[critical]` · K03 scope persists after feature de-provision `[critical]` · K04 revoked key dead `[critical]`
- [ ] PRIV‑P01 cross-project session replay `[critical]` · P02 project-pinned key confinement `[critical]` · P03 disable→instant revocation `[critical]` · P04 tenant ban cross-project `[critical]` · P05 anonymous boundary · P06 forged/tampered JWT `[critical]` · P07 expired access token

**PRIV‑K01** `[critical]` with `{{A_PUB}}` call `POST /leaderboards/{id}/scores`, `POST /server/player-sessions/verify`, `POST /fleets/heartbeat` → `403` each (secret required).
**PRIV‑K02** `[critical]` with a key lacking a scope, call the gated route: no `matchmaker` → `POST /matchmaker/tickets` `403`; no `fleet`/`p2p_relay` → those routes `403` (or `404` if unmounted locally — record which).
**PRIV‑K03** `[critical]` a key with `matchmaker`/`p2p_relay` scope that works; revoke the backing feature grant (as PA) and/or flip the feature switch off; re-call. Secure = denied. **Known behavior:** there is a bounded (~5s) authorizer-cache allow window after revocation; a call succeeding *within* that window is intended, but a call succeeding *after* the cache refresh is a finding.
**PRIV‑K04** `[critical]` revoke `KEY‑throwaway` in CP, then immediately reuse it → `403` on the very next call.
**PRIV‑P01** `[critical]` present a **P1** session token with the **P2-pinned** key `{{A_PUB_P2}}` → `403` (JWT `project_id` must match a project-pinned key). Portability across projects is critical.
**PRIV‑P02** `[critical]` with `{{A_PUB}}` (pinned P1) present a session from another project → `403`; confirm a `project_id=0` session (if craftable) does not short-circuit the check.
**PRIV‑P03** `[critical]` capture A‑DISABLED's valid token *before* disabling; disable (CP); reuse on `GET /profile` and `POST /server/player-sessions/verify` → rejected immediately (`session_epoch` bumped), not valid until natural expiry.
**PRIV‑P04** `[critical]` A‑BANNED banned at tenant level (needs a linked account); `POST /server/player-sessions/verify` with `{{A_SEC}}` for that player in **every** Tenant A project → opaque `401` in all.
**PRIV‑P05** with `{{ANON_SESSION}}` hit friends/invites/account-linked features → `403` where a linked account is required.
**PRIV‑P06** `[critical]` take A1's token, flip a claim (`player_id`/`tenant_id`/`project_id`) without re-signing; also sign one with a wrong key → `401` each.
**PRIV‑P07** mint/wait an expired access token → `GET /profile` `401`, `WWW-Authenticate: ... token expired`.
[ ] pass [ ] fail  notes: ______

---

## 15. Tenant isolation (priority)

Method: create the **same-named** resource in A and B; then, with **A creds only**, (a) list → A rows only; (b) direct-get B's id → deny; (c) mutate B's id → deny + verify no write. Enumerate sequential ids for IDOR. Focus on **un-RLS'd** tables: `player_accounts`, `feature_grants`, `tenant_player_bans`, `tenant_signup_requests`.

**Coverage index**
- [ ] ISO‑01 storage `[critical]` · 02 leaderboards `[critical]` · 03 game sessions `[critical]` · 04 matchmaker tickets `[critical]` · 05 friends/remote-addrs `[critical]` · 06 profile/player enumeration `[critical]` · 07 server verify foreign token `[critical]` · 08 key/tenant confusion `[critical]` · 09 control-panel isolation (=PRIV‑D10) `[critical]` · 10 RLS fail-closed sanity `[critical]`

**ISO‑01** A1 & B1 both wrote `save1`; with A creds read `save1` (A1's only) and attempt to read/overwrite B1's by any constructable key → only A data; B's row unchanged.
**ISO‑02** with A creds `GET /leaderboards/{{B_LB_ID}}/top` + `/around-me`; `{{A_SEC}}` `POST /leaderboards/{{B_LB_ID}}/scores` → deny/empty; no B entry written. *(Note: foreign leaderboard reads return `200 {"entries":[]}` under RLS — enumeration-resistant, not a leak. A `200` with **actual B rows** is critical.)*
**ISO‑03** B1 creates a session; with A session `GET /game-session?joinCode=<B>`, `GET /game-session/<Bid>`, `POST …/join` → deny/404.
**ISO‑04** B1 creates a ticket; with `{{A_MM}}` + A session `GET`/`DELETE /matchmaker/tickets/<Bid>` → `404`; B ticket intact.
**ISO‑05** `[critical]` as A1 `POST /friends/{{B1_PLAYER_ID}}/request`, `GET /friends/{{B1_PLAYER_ID}}/remote-addrs`, and `GET /server/players/{{B1_PLAYER_ID}}/remote-addrs` with `{{A_SEC}}` → deny/404; **no** B addresses or account existence leaked (global `player_accounts` has no RLS — probe several B ids).
**ISO‑06** `[critical]` walk sequential B player ids across every `player_id` endpoint → uniform deny/404, no oracle distinguishing "B player exists" vs "no such id". *(Prior pass note: `/friends/{id}/block` and `/unblock` returned `500` on foreign/nonexistent ids — not a leak but should be an opaque deny; re-check.)*
**ISO‑07** `[critical]` `POST /server/player-sessions/verify` with `{{A_SEC}}` but a **B1** token in the body → opaque `401`. Returning B1's identity is critical.
**ISO‑08** `[critical]` present `{{B_PUB}}` with `{{A1_SESSION}}` (mismatched tenants) → `403`.
**ISO‑09** = PRIV‑D10 (CP data isolation).
**ISO‑10** `[critical]` (`make psql`) as `ggscale_app` without setting `app.tenant_id`: `SET ROLE ggscale_app; RESET app.tenant_id; SELECT count(*) FROM project_players;` → `0`. Non-zero = RLS fail-open. (`tenants` intentionally returns rows — control-plane metadata, not player-data RLS.)
[ ] pass [ ] fail  notes: ______

---

## 16. Edge cases, validation & abuse

**Coverage index**
- [ ] EDGE‑01 login lockout/limiter · 02 CSRF on CP + player mutations · 03 validation sweep · 04 oversized payloads · 05 malformed input · 06 injection stored & escaped · 07 unicode/RTL/NUL · 08 pagination · 09 methods/CORS `[prod-smoke]` · 10 idempotency/double-submit · 11 TTL expiries

**EDGE‑01** (destructive, throwaway account) many failed logins from one IP: API auth limiter throttles quickly (`429 retry_after_seconds`), refusing even a correct password while active; CP login locks after ~10 failures / 15-min window. Confirm recovery after the window.
**EDGE‑02** strip CSRF from a CP mutation and a player-site mutation → `403` each.
**EDGE‑03** invalid emails; xuid > 64 / control chars; `max_players` 0/‑1/1e9; matchmaker `string_properties` > 16 or value > 128B or reserved `region`; empty required fields → `4xx`, no `500`.
**EDGE‑04** oversized storage value (> configured storage cap; current local default is 1 MiB) → `413`; long labels/display names bounded. *(Confirmed fix — REG‑05.)*
**EDGE‑05** `Content-Type: text/plain` with JSON body → `415`; truncated JSON → `400`; invalid storage JSON → `400`.
**EDGE‑06** SQL-ish (`'; DROP TABLE project_players;--`) and HTML payloads into display_name/xuid/key label/leaderboard name/remote-addr → stored verbatim (parameterized), rendered **escaped** everywhere (ties to PUI‑08).
**EDGE‑07** emoji, combining marks, RTL override, NUL byte in text fields → consistent handling, never corrupt or `500`.
**EDGE‑08** create > page-size storage objects/friends; page via cursor/limit → no dupes/skips, stable order, and an A cursor never returns B rows.
**EDGE‑09** `[prod-smoke]` `DELETE` a GET-only route → `405`; check CORS. **On production confirm `ENV=production` rejects `*`/empty origins and only echoes the configured allowlist** (Local intentionally returns wildcard — see §17).
**EDGE‑10** accept the same friend request twice (`409`), join a session twice (idempotent roster), submit the same invite twice → no duplicate rows.
**EDGE‑11** verify code past 15 min → invalid; game invite past ~5 min → gone; access token past ~15 min → `401`. *(Prior pass flagged an expired-verify-code edge — REG‑06; re-confirm the fix's boundary: expired codes for **unverified** accounts must reject, and idempotent re-verify of an already-verified account must not mint a session.)*
[ ] pass [ ] fail  notes: ______

---

## 17. Production smoke subset  `[prod-smoke]` — read-only

> **BLOCKED until provided:** the real `{{PROD_HOST}}` and the expected CORS origin allowlist (allowed origins + at least one origin that must be rejected). Without these, EDGE‑09 prod cannot assert. Do not run writes, bans, disables, deletes, seed, or lockout against production.

Run **only** these, all read-only:
- SMOKE‑01..04, AUTH‑11, CP‑02, PUI‑02, EDGE‑09.
- **Isolation reads (GET-only):** ISO‑01/02/03/04/05/06 in their *read* form — attempt cross-tenant GETs and confirm deny; **skip** all cross-tenant PUT/POST/DELETE.
- PRIV‑D09/D10 navigate-only (no mutation).
- Production config confirmations: Secure cookies set + HttpOnly; HTTPS enforced/redirect; real SMTP delivers a verification email to a mailbox you control; `ENV=production` CORS validation (EDGE‑09) rejects `*` and unlisted origins, allows a listed one.

If any cross-tenant GET returns another tenant's data on production, **stop and report immediately.**

---

## 18. Regression re-verify (prior confirmed findings)

Cheap, high-value: re-run the exact repro for each finding fixed in the last pass so a regression can't slip back in. All should now **PASS**.

| ID | Prior finding (now fixed) | Re-verify | Expect |
|----|---------------------------|-----------|--------|
| **REG‑01** | AUTH‑03: unverified email could log in | signup (no verify) → `POST /auth/login` | `403 "email not verified"` |
| **REG‑02** | PRIV‑D06: tenant admin created secret keys | A‑ADMIN `POST …/api-keys key_type=secret` | `403`; publishable → `200` |
| **REG‑03** | PRIV‑D07: tenant admin created team invites | A‑ADMIN `POST …/team/invite` | `403`; owner → `200` |
| **REG‑04** | PRIV‑D08: tenant admin granted fleet_operator | A‑ADMIN `POST …/team/members/{uid}/roles grant fleet_operator` | `403`; owner → `200` |
| **REG‑05** | EDGE‑04: storage accepted values over the configured cap | `PUT /storage/objects/{k}` > configured cap (current local default 1 MiB) | `413`, nothing stored |
| **REG‑06** | EDGE‑11: expired verify code "verified" | verify with an expired code on an **unverified** account | `400`; and idempotent re-verify of an already-verified account mints **no** session |
| **REG‑07** | MM policy drift + missing worker grant | boot stack; `make logs \| grep -c "permission denied"` = 0; create+match two tickets (MM‑01/02) | 0 denied; `matched` with shared `match_id` |
| **REG‑08** | server-verify foreign-token (Casbin `player:verify`) | ISO‑07 own-token `200`, foreign-token opaque `401` | as stated |
| **REG‑09** | PRIV‑K03: `feature_grants_feature_check` rejected `matchmaker`, so deprovision was unrepresentable (fixed in migration 0007) | insert an `enabled=false` matchmaker grant (as PA / via psql), wait past the authorizer cache window, create a ticket with a matchmaker-scoped key | insert succeeds; ticket → `403` |
| **REG‑10** | ISO‑06: `/friends/{id}/block` and `/unblock` returned `500` on a nonexistent id | as A1, `POST /friends/999999999/block` and `/unblock` | opaque `404` each, matching the friend-request deny |

[ ] pass [ ] fail  notes: ______

---

## 19. Severity & triage

| Severity | Definition | Action |
|----------|-----------|--------|
| **Critical — release blocker** | Any cross-tenant data read/write (ISO‑*), privilege escalation (PRIV‑*/LINK‑09/SIGNUP‑10), auth bypass, tampered-token acceptance, publishable key doing server-authoritative writes, secret-key creation by non-owner, feature self-grant, tenant-signup hijack (SIGNUP‑07), player-link rebind onto another account (LINK‑07), RLS fail-open | **Stop, record full repro (request + response + IDs), report immediately.** |
| **High** | Missing revocation (disabled/banned token still valid), CSRF not enforced, stored XSS, refresh reuse after rotation, invite throttle bypass, enumeration oracle on signup/friend search | Record with repro; fix before release. |
| **Medium** | Validation gaps, `500`s on bad input, weak rate limiting, pagination leaks within a tenant, inconsistent deny codes | Record; batch for triage. |
| **Low** | Cosmetic, unclear errors, minor UX | Record; non-blocking. |

For every fail capture: test ID, env, exact request (headers/body) or UI steps, actual vs expected, affected IDs/tenants. For browser findings, save a screenshot/GIF.

---

## 20. Results log

**Initial local run summary (2026-07-14 13:46 America/Chicago):** 135 logged checks, 131 passed, 3 failed, 1 blocked. The three local failures (PRIV-K03, ISO-06, and EDGE-05) were fixed test-first and pass in the branch-follow-up results below. **Current blocker:** production smoke requires `{{PROD_HOST}}` and the expected CORS allowlist.

| Test ID | Env | Result | Severity | Notes / repro |
|---------|-----|--------|----------|---------------|
| P0-SEED | Local | PASS | - | `make seed` completed 2026-07-14; seed produced 4 tenants, 12 projects, 442 players, 690 scores, 151 friends, 75 storage rows, 165 tickets. |
| P0-FIXTURES | Local | PASS | - | Seed shape is realistic for this plan: A/P1 has 30 players, 17 linked+verified and 13 unverified/unlinked; B/P1 has 41 players, 27 linked+verified and 14 unverified/unlinked. Plaintext API keys were created through CP, not DB. |
| P0-FRESH-KEYS | Local | PASS | - | Continuation suite created fresh CP-backed API keys, plaintext omitted: A_PUB_RUN=41, A_SEC_RUN=42, A_PUB_P2_RUN=43, A_NOMM_RUN=44, B_PUB_RUN=45, B_SEC_RUN=46. A_NOMM_RUN had managed scopes stripped through CP Features for scope-denial tests. |
| P0-CONT-FIXTURES | Local | PASS | - | Continuation suites created fresh CP-backed keys and throwaway projects for isolated invite-limit and disabled-player checks. Plaintext keys omitted. |
| P0-API-RT-FIXTURES | Local | PASS | - | API/realtime continuation created fresh CP-backed publishable and secret keys for remaining API, expiry, and WebSocket checks. Plaintext omitted. |
| P0-FINAL-FIXTURES | Local | PASS | - | Final local suite created fresh CP-backed keys for remaining destructive/special checks: FINAL_PUB_P1=60, FINAL_SEC_P1=61, FINAL_PUB_P2=62, FINAL_SEC_P2=63. Plaintext omitted. |
| P0-CUSTOM-TOKEN-FIXTURE | Local | PASS | - | Tenant A local test fixture was configured with a custom-token signing secret for AUTH-13 because seed data left `custom_token_secret` unset. |
| BROWSER-SETUP | Local | PASS | - | In-app Browser connector unavailable (`agent.browsers.list()` returned `[]`), but local Playwright Chromium launch works with approved outside-sandbox execution. Screenshot-backed browser suite passed with evidence under `/private/tmp/ggscale-preprod-screenshots`. |
| ENV-REBUILD | Local | PASS | - | During browser testing, source exposed `/control-panel/request-access` but the running 3-day-old container returned route/config-mismatched output. Ran `make up` on 2026-07-14 to rebuild/recreate `ggscale-server`; health returned 200 afterward. |
| SIGNUP-05-SETUP | Local | PASS | - | Platform admin `admin@demo.ggscale` verified via Mailpit and enabled public tenant signup at `/admin/tenant-signups/config`; `/request-access` had previously returned the closed-signups page. |
| SMOKE-01 | Local | PASS | - | `GET /v1/healthz` -> 200, `{"status":"ok","version":"v1","commit":"unknown"}`, `X-Api-Version: v1`. |
| SMOKE-02 | Local | PASS | - | Playwright rendered CP login form with no harness-captured console/page/request failures. Screenshot: `/private/tmp/ggscale-preprod-screenshots/01-control-panel-login.png`. |
| SMOKE-03 | Local | PASS | - | Playwright rendered player login form + signup link with no harness-captured console/page/request failures. Screenshot: `/private/tmp/ggscale-preprod-screenshots/02-player-login.png`. |
| SMOKE-04 | Local | PASS | - | `GET /v1/does-not-exist` -> 404 plain `404 page not found`, no stack trace. |
| SMOKE-05 | Local | PASS | - | `GOCACHE=/private/tmp/ggscale-go-build go run ./cmd/openapi-dump /private/tmp/ggscale-openapi.yaml`; `diff -u openapi.yaml /private/tmp/ggscale-openapi.yaml` had no diff. |
| CP-01 | Isolated local | PASS | - | Playwright ran first-run `/control-panel/setup` against a clean temporary compose stack on port `18080`: bad token rejected; bootstrap token accepted; first admin `cp01-admin-1784055028865@example.com` created and Mailpit-verified; `/setup` and token reuse returned 410 after completion; DB had exactly one verified platform admin. Screenshots: `/private/tmp/ggscale-preprod-screenshots/46-cp01-setup-token.png`, `/private/tmp/ggscale-preprod-screenshots/47-cp01-create-admin.png`, `/private/tmp/ggscale-preprod-screenshots/48-cp01-verified-home.png`. |
| CP-02 | Local | PASS | - | A-OWNER and B-OWNER login required Mailpit verification on first login, then CP home rendered tenant list. |
| CP-02-browser | Local | PASS | - | Playwright owner login reached tenant list and logout returned to login page. Screenshots: `/private/tmp/ggscale-preprod-screenshots/03-cp-owner-home.png`, `/private/tmp/ggscale-preprod-screenshots/08-cp-after-logout.png`. |
| CP-03 | Local | PASS | - | Fresh CP throwaway user enabled 2FA, completed TOTP challenge with trust-device, regenerated backup codes, used one backup code once, verified replay denial, and disabled 2FA. Screenshots: `/private/tmp/ggscale-preprod-screenshots/28-cp-2fa-setup.png`, `/private/tmp/ggscale-preprod-screenshots/29-cp-2fa-backup-codes.png`, `/private/tmp/ggscale-preprod-screenshots/30-cp-2fa-challenge.png`, `/private/tmp/ggscale-preprod-screenshots/31-cp-2fa-disabled.png`. |
| CP-04 | Local | PASS | - | Playwright owner created a new tenant through CP; DB shows the tenant plus one starter project and one first API key. Screenshot: `/private/tmp/ggscale-preprod-screenshots/42-cp-create-tenant.png`. |
| CP-05 | Local | PASS | - | Playwright owner created an additional project in Tenant A through CP. Screenshot: `/private/tmp/ggscale-preprod-screenshots/43-cp-create-project.png`. |
| CP-05-duplicate | Local | PASS | - | Duplicate project name `Starfall Arena` in Tenant A returned 409 and DB still has exactly one live row with that name. |
| CP-06 | Local | PASS | - | Created A/B publishable and secret keys through CP; revoked KEY_throwaway `api_key_id=39`; revoked key immediately returned 403 on `/auth/anonymous`. |
| CP-06-browser | Local | PASS | - | Playwright rendered API key list with created fixture keys visible. Screenshot: `/private/tmp/ggscale-preprod-screenshots/05-cp-api-keys.png`. |
| CP-07 | Local | PASS | - | A-OWNER invited `admin-a@demo.ggscale` as `tenant_admin` and `member-a@demo.ggscale` as `tenant_member`; both accepted magic links with password + CSRF and received sessions. |
| CP-07-browser | Local | PASS | - | Playwright rendered Team page with accepted `admin-a@demo.ggscale` and `member-a@demo.ggscale`. Screenshot: `/private/tmp/ggscale-preprod-screenshots/06-cp-team.png`. |
| CP-07-complete | Local | PASS | - | Focused Playwright/request suite completed the positive team-management gaps: owner granted and revoked `role:fleet_operator` for `cp07-member-1784054741225@example.com`, removed that member and verified tenant access denied, then revoked pending invite `15`. Screenshot: `/private/tmp/ggscale-preprod-screenshots/44-cp-team-role-remove-revoke.png`. |
| CP-08-list | Local | PASS | - | Playwright rendered Players list and searched for seeded A1. Screenshot: `/private/tmp/ggscale-preprod-screenshots/07-cp-players.png`. |
| CP-08-complete | Local | PASS | Critical | Focused Playwright/request suite completed player admin gaps on throwaways: accepted a CP player invite for `cp08-invite-1784054741225@example.com`; rendered detail for verified API player `cp08-api-1784054741225@example.com`; CP disable invalidated its live API token; re-enable restored login; tenant ban persisted for the linked throwaway. Throwaway API key `78` revoked. Screenshot: `/private/tmp/ggscale-preprod-screenshots/45-cp-player-detail-disable-ban.png`. |
| AUTH-01 | Local | PASS | - | `POST /auth/anonymous` with A_PUB -> 200, anonymous `player_id=898`. |
| AUTH-02 | Local | PASS | - | Signup `newp-20260714a@example.com` -> 202; Mailpit code verify -> 200; subsequent login -> 200. |
| AUTH-03 / REG-01 | Local | PASS | Critical | Signup `unverified-20260714a@example.com`, no verify; login -> 403 `email not verified`. |
| AUTH-04 | Local | PASS | - | A1 wrong password -> 401 `invalid credentials`. |
| AUTH-05 | Local | PASS | - | Duplicate signup `dup-<run>@example.com`: both requests returned generic 202; DB check found exactly one A/P1 `project_players` row. |
| AUTH-06 | Local | PASS | - | Signup with a 100-byte ASCII password returned 400, not 500 and not silent truncation. |
| AUTH-07 | Local | PASS | - | Wrong verify code `000000` for a fresh unverified account returned 400. |
| AUTH-08/09 | Local | PASS | Critical | A1 refresh rotated -> 200 new pair; old refresh reuse -> 401 `invalid refresh`. |
| AUTH-10 | Local | PASS | - | Logout with new refresh -> 204; subsequent refresh -> 401. Original access token was also rejected as `session revoked`, stronger than required. |
| AUTH-11 | Local | PASS | - | No Authorization -> 401; `Bearer garbage` -> 401; revoked KEY_throwaway -> 403. |
| AUTH-12 | Local | PASS | - | `GET /profile` with A_PUB and no `X-Session-Token` -> 401. |
| AUTH-13 | Local | PASS | Critical | With Tenant A custom-token fixture secret configured, valid HS256 token (`aud=ggscale-custom-token`, `exp`, `external_id`) minted a session; same claims signed with wrong key returned 401. |
| AUTH-14 | Local | PASS | - | Fresh signup hit five wrong-code verification attempts returning 400; sixth wrong attempt returned 429, and the real Mailpit code was still refused during lockout. |
| PROF-01 | Local | PASS | - | `GET /profile` as A1 -> 200 with own A/P1 profile. |
| PROF-02/04 | Local | PASS | - | `PATCH /profile` xuid persisted and re-read correctly; 65-char xuid, control-char xuid, and empty patch all returned 400. |
| PROF-03 | Local | PASS | - | Profile email change verified via Mailpit; subsequent `GET /profile` returned the new email with `email_verified_at` set. |
| PUI-02-browser | Local | PASS | - | Playwright player login reached account home. Screenshot: `/private/tmp/ggscale-preprod-screenshots/09-player-account-home.png`. Player logout assertion was removed from this screenshot suite after route-rate-limit/retry instability. |
| PUI-06-browser | Local | PASS | - | Playwright opened remote-address UI and captured configured LAN address. DB verification: `remote_addr_ip_lan=192.168.1.50:9000`. Screenshots: `/private/tmp/ggscale-preprod-screenshots/10-player-remote-addrs.png`, `/private/tmp/ggscale-preprod-screenshots/11-player-remote-added.png`. |
| PUI-01 | Local | PASS | Critical | Playwright created a fresh global player account, verified the Mailpit email code, reached account home, and DB shows `email_verified_at IS NOT NULL`. Screenshot: `/private/tmp/ggscale-preprod-screenshots/32-player-signup-verified.png`. |
| PUI-03 | Local | PASS | Critical | Playwright player 2FA flow used a seeded verified account to enable TOTP, complete the TOTP challenge, regenerate backup codes, use one backup code once, verify replay denial, and disable 2FA. Screenshots: `/private/tmp/ggscale-preprod-screenshots/33-player-2fa-setup.png`, `/private/tmp/ggscale-preprod-screenshots/34-player-2fa-backup-codes.png`, `/private/tmp/ggscale-preprod-screenshots/35-player-2fa-challenge.png`, `/private/tmp/ggscale-preprod-screenshots/36-player-2fa-disabled.png`. |
| PUI-04 | Local | PASS | - | Playwright/browser-context flow covered friend request by email, incoming accept, block, and unblock with DB state checks around rate-limited local POSTs. Screenshot: `/private/tmp/ggscale-preprod-screenshots/37-player-friends-accepted.png`. |
| PUI-07 / PUI-05-current | Local | PASS | Critical | Control-panel player invite was accepted by a new player and linked to project 13. The old public join-by-ID flow is absent in current code (`players never self-join`); invite acceptance is the current project-join path. Screenshots: `/private/tmp/ggscale-preprod-screenshots/40-player-invite-accept.png`, `/private/tmp/ggscale-preprod-screenshots/41-player-invite-linked.png`. |
| PUI-08 / EDGE-06 | Local | PASS | Critical | Stored display-name payload `<img src=x onerror=...>` rendered as inert text on account home and friends list with no browser dialogs; raw HTML was escaped. Screenshots: `/private/tmp/ggscale-preprod-screenshots/38-player-xss-account.png`, `/private/tmp/ggscale-preprod-screenshots/39-player-xss-friends.png`. |
| SIGNUP-01-browser | Local | PASS | - | After platform admin enabled public signup, Playwright rendered tenant signup request form. Screenshot: `/private/tmp/ggscale-preprod-screenshots/13-tenant-signup-form.png`. |
| SIGNUP-02-browser | Local | PASS | - | Playwright submitted tenant signup request and saw generic `Request received` acknowledgement. Screenshot: `/private/tmp/ggscale-preprod-screenshots/14-tenant-signup-ack.png`; DB has pending browser signup request rows. |
| SIGNUP-04 | Local | PASS | - | Browser validation rejected a one-character tenant name on `/request-access` without a 500. |
| SIGNUP-05 | Local | PASS | - | Platform-admin review page `/admin/tenant-signups` rendered pending requests and the public signup toggle; toggle enabled state was verified. |
| SIGNUP-06 | Local | PASS | Critical | Platform admin approved `tenant-approve-1784047016095@example.com`; Mailpit delivered approval email with a relative accept link; Playwright accepted it as a new user, DB shows `status=accepted`, `tenant_id=9`, and one owner membership. Screenshot: `/private/tmp/ggscale-preprod-screenshots/15-tenant-signup-accepted.png`. |
| SIGNUP-03 | Local | PASS | Critical | Duplicate tenant-signup email submitted twice yielded one pending request. Taken public tenant name returned 422 with no request row; this matches current product behavior because tenant names are public/non-secret. |
| SIGNUP-07 | Local | PASS | Critical | Fresh existing control-panel user path required current password: wrong password returned 401 and left request `approved` with no tenant; correct password accepted and created the owner tenant. |
| SIGNUP-08 | Local | PASS | - | Platform admin denied `tenant-deny-1784047016095@example.com`; DB shows `status=denied` and no `tenant_id`; Mailpit delivered denial email with the provided reason. |
| SIGNUP-09 | Local | PASS | - | Platform admin disabled public signup; `/request-access` rendered the closed-signups page. Re-enabled afterward for subsequent local testing. Screenshot: `/private/tmp/ggscale-preprod-screenshots/16-tenant-signup-disabled.png`. |
| SIGNUP-10 | Local | PASS | Critical | A-OWNER non-platform admin GET `/admin/tenant-signups` and forged POSTs to config/approve/deny all returned 403. |
| SIGNUP-11 | Local | PASS | Critical | Tenant-signup accept links are single-use; reused accepted code and tampered code returned 404. A DB-expired approved code returned 410 and did not create a tenant. |
| LINK-01/02 | Local | PASS | - | Direct link-player dialog for unverified A/P1 player 457 rendered read-only external ID `player-6e1eba3f-1`; verified A1 search did not expose a Link action. Screenshot: `/private/tmp/ggscale-preprod-screenshots/17-link-player-dialog.png`. |
| LINK-03 | Local | PASS | Critical | A-OWNER linked fresh email `link-target-1784047851307@example.com` onto existing `project_players.id=457`; DB shows the same row now verified+linked and exactly one project row for that email. |
| LINK-04 | Local | PASS | Critical | Attempting to link unverified player 458 to A2's existing project email returned 409 and left target row email/account/verification unset. |
| LINK-05/06 | Local | PASS | - | Resending link invite for player 460 revoked the prior open invite and left exactly one open invite; player list page 2 shows `INVITE PENDING` badge for player 460. Screenshot: `/private/tmp/ggscale-preprod-screenshots/18-link-invite-pending.png`. |
| LINK-07 | Local | PASS | Critical | After link invite was sent for player 461, target row was linked to another account before accept; accepting stale invite rendered conflict and DB row remained `prelinked-1784047851307@example.com` linked to the original account. |
| LINK-08 | Local | PASS | - | Tampered player invite/link code returned 404; no bind occurred. |
| LINK-09 | Local | PASS | Critical | A-MEMBER GET link dialog returned 403; A-ADMIN own-tenant dialog returned 200; A-ADMIN forged Tenant B link POST returned 403. |
| INV-01 | Local | PASS | - | Two back-to-back link re-invites to `invite-burst-1784047851307@example.com` succeeded; third within cooldown returned 429 with `Retry-After=600`. |
| INV-02 | Local | PASS | - | Fresh throwaway project default inviter budget allowed 10 distinct player-link invites from one inviter; 11th returned 429. |
| INV-03/06 | Local | PASS | - | Tenant owner could set per-project invite quotas. Over-default values did not persist; tightened `domain_per_day=1` on a throwaway project made the second invite return 429. |
| INV-04/05/07 | Local | PASS | Critical | A-OWNER forged tenant API ceiling and recipient override writes returned 403. PA one-sided zero recipient override did not persist; PA burst=5 allowed five same-recipient link re-invites; both-zero cleared the override. |
| INV-08-team | Local | PASS | Critical | Team invite accepted once; reused accepted code and tampered code returned 404; DB-expired pending team invite returned 410. Tenant-signup invite expiry/single-use/tamper is covered by SIGNUP-11. |
| INV-08-player | Local | PASS | Critical | Player invite accepted once; reused accepted code and tampered code returned 404; DB-expired pending player invite returned 410. Link-invite tamper/target-conflict paths are covered by LINK-07/08. |
| FRND-01/02/03/09 + PRES-01/02 | Local | PASS | Critical | A1 -> A3 friend request/accept succeeded; A3 presence update appeared in A1 accepted-friends list; accepted friend remote address was readable. |
| FRND-04/05/06/07/08/10 | Local | PASS | Critical | A1/A4 reject, block, blocked re-request hiding, unblock, accept, and delete passed; anonymous friend request returned 403; foreign/B1 remote-addrs probe returned deny. |
| SESS-01/02/03/04/05/06/07/09 | Local | PASS | - | Created game session `gs_838d632c79b0096bbc4aca541af73ba9`, resolved join code, A3 joined, heartbeat returned peers, friend invite succeeded, non-friend invite returned 403, negative/huge `max_players` returned 400, host end returned 204. |
| SESS-08 / EDGE-11-game-invite | Local | PASS | - | Game invite id 2 was visible to the recipient before forced expiry and absent from `GET /invite` after `expires_at` was moved into the past. |
| STOR-01/02/03 | Local | PASS | - | PUT/GET/list `preprod-save1` as A1 returned versioned caller-owned object. |
| STOR-04 | Local | PASS | Critical | `If-Match: 1` update -> 200 version 2; repeat `If-Match: 1` -> 412 `version mismatch`. |
| STOR-05/07 | Local | PASS | - | Invalid JSON storage PUT returned 400; delete target PUT -> 200, DELETE -> 204, subsequent GET -> 404. |
| STOR-06 / EDGE-04 / REG-05 | Local | PASS | - | 70,011-byte value stored under current 1 MiB cap. 1,100,011-byte value returned 413 `value exceeds maximum size`. Plan updated from stale 64 KiB expectation. |
| LB-01 | Local | PASS | - | `GET /leaderboards/31/top` with A_PUB + A1 session -> 200 with A rows. |
| LB-02 | Local | PASS | - | `GET /leaderboards/31/around-me?radius=2` returned `self_rank=0` and three surrounding entries for A1. |
| LB-03 | Local | PASS | Critical | `POST /leaderboards/31/scores` with A_SEC + A1 session -> 201. |
| LB-04 / PRIV-K01 | Local | PASS | Critical | Same score submit with A_PUB -> 403. Publishable server-verify -> 403. Fleet heartbeat is unmounted locally (`404`). |
| LB-05 | Local | PASS | - | Re-read leaderboard 31 showed A1 score 999999 at rank 0. |
| MM-00 | Local | PASS | - | Fresh publishable keys have `scopes={matchmaker}`; A_MM created ticket 337 with 201 queued. |
| MM-01/02/03/04/05 / PRIV-K02 / REG-07 | Local | PASS | Critical | A1/A3 matchmaker tickets 338/339 matched with shared match_id `mm_37525fd413434f4e`; queued ticket cancel returned 204; no-matchmaker-scope key returned 403 after cache window; too many/reserved string properties returned 400. |
| WS-01/02 | Local | PASS | Critical | Go WebSocket probe authenticated to `/v1/ws` with API key + player session, opened four concurrent connections, and the fifth returned 503 per-player cap (`opened=4 cap_status=503`). |
| REG-07-LOGS | Local | PASS | - | `docker compose logs ggscale-server` contained 0 occurrences of `permission denied for table matchmaking_tickets`. |
| GO-TEST | Local | PASS | - | `GOCACHE=/private/tmp/ggscale-go-build go test ./...` passed across all packages. |
| PRIV-K04 | Local | PASS | Critical | Revoked KEY_throwaway `api_key_id=39` immediately denied `/auth/anonymous` with 403. |
| PRIV-K03 | Local | FAIL | Critical | Attempted matchmaker feature deprovision after confirming a scoped key could create a ticket. DB rejected `feature='matchmaker'` with `feature_grants_feature_check` even though RBAC code defines `FeatureMatchmaker` and comments say explicit `enabled=false` rows disable it. Deprovision cannot be represented, so the deprovision persistence check cannot pass. **Fixed 2026-07-14** (migration `0007_feature_grants_matchmaker` widens the constraint; guarded by integration tests) — re-verify as REG‑09. |
| PRIV-P01/P02/P06 | Local | PASS | Critical | P1 session with A/P2-pinned key returned 403; A session with B key returned 403; unsigned JWT claim tamper returned 401. |
| PRIV-P05 | Local | PASS | - | Anonymous player 930 was denied account-linked friends and game-invite surfaces. |
| PRIV-P07 / EDGE-11-access-token | Local | PASS | Critical | Crafted expired JWT using the local server signing key returned 401 and a `WWW-Authenticate` token-expired hint. |
| ISO-01 | Local | PASS | Critical | A1 and B1 wrote the same storage key `iso-save1`; A1 read returned only the A value. |
| ISO-02 | Local | PASS | Critical | A creds reading B leaderboard 39 returned 200 `{"entries":[]}`; A_SEC posting score to B leaderboard returned 404 `leaderboard not found`. |
| ISO-03 | Local | PASS | Critical | B1 created a game session; A1 resolving B join code and joining by B session id both returned 404. |
| ISO-04 | Local | PASS | Critical | B1 created matchmaker ticket; A1 GET/DELETE of B ticket id returned 404. |
| ISO-05 | Local | PASS | Critical | A1 friend request to B1 returned 404; A_SEC server remote-address read for B1 returned 404, no B addresses leaked. |
| ISO-06 | Local | FAIL | Medium | Reconfirmed prior opaque-denial bug: A1 `POST /friends/999999999/block` returned 500 `{"title":"Internal Server Error","status":500,"detail":"internal error"}` instead of uniform 404/403. No cross-tenant data leaked, but bad ID input still produces a 500. **Fixed 2026-07-14** (block/unblock now return the same opaque 404 as friend requests; covered by an integration test) — re-verify as REG‑10. |
| ISO-07 / REG-08 | Local | PASS | Critical | A_SEC verifying A1 `session_token` -> 200 identity; A_SEC verifying B1 `session_token` -> opaque 401 `invalid session`. |
| ISO-08 | Local | PASS | Critical | B_PUB with A1 session on `/profile` -> 403. |
| ISO-09 / PRIV-D10 | Local | PASS | Critical | A-OWNER direct GET Tenant B CP projects and API keys -> 403, no B data rendered. |
| ISO-10 | Local | PASS | Critical | `SET ROLE ggscale_app; RESET app.tenant_id; SELECT count(*) FROM project_players;` -> 0. |
| PRIV-D01 | Local | PASS | Critical | A-MEMBER forged publishable API-key create -> 403. |
| PRIV-D02 | Local | PASS | - | A-MEMBER forged project create -> 403. |
| PRIV-D03 / PRIV-D08 | Local | PASS | Critical | A-MEMBER forged team invite, role-change, and remove-member POSTs returned 403. A-ADMIN forged `fleet_operator` grant returned 403. |
| PRIV-D04 / PRIV-D05 / PRIV-D12 | Local | PASS | Critical | A-MEMBER forged player disable/ban, tenant settings, project settings, and project rate-limit mutations returned 403. A-ADMIN could disable a player; owner re-enabled the fixture, confirming project/player manage is the effective rule. |
| PRIV-D06 / REG-02 | Local | PASS | Critical | A-ADMIN forged secret-key create -> 403; A-ADMIN publishable key create -> 200 (`api_key_id=40`). |
| PRIV-D07 / REG-03 | Local | PASS | Critical | A-ADMIN forged team invite -> 403; owner invite path already passed in CP-07. |
| PRIV-D09 | Local | PASS | Critical | A-OWNER direct GET `/admin/users` and `/admin/tenant-signups` -> 403. |
| PRIV-D11 | Local | PASS | - | Granted `role:fleet_operator` to A-MEMBER; local fleet surface denied/unmounted (`403`/`404`) with `FEATURE_FLEET_ENABLED=false`, and the role still could not mutate projects, team, or API keys. Grant was revoked after the check. |
| PADMIN-01 | Local | PASS | - | Playwright platform users list rendered; platform admin disabled a throwaway control-panel user, blocked login, then re-enabled the user. Screenshot: `/private/tmp/ggscale-preprod-screenshots/21-platform-users.png`. |
| PADMIN-02 | Local | PASS | - | Playwright global player-account list/detail rendered; disabling the throwaway account blocked player login and enabling restored it. Screenshots: `/private/tmp/ggscale-preprod-screenshots/22-platform-player-accounts.png`, `/private/tmp/ggscale-preprod-screenshots/23-platform-player-account-detail.png`. |
| PADMIN-03 | Local | PASS | - | Platform admin invited and accepted a platform-admin candidate through Mailpit magic link; DB shows `is_platform_admin=true`, and the user can reach `/admin/users`. |
| PADMIN-04 | Local | PASS | - | Platform `/admin/settings` and `/admin/plugins` rendered without error. Screenshots: `/private/tmp/ggscale-preprod-screenshots/19-platform-server-settings.png`, `/private/tmp/ggscale-preprod-screenshots/20-platform-plugins.png`. |
| PADMIN-05 | Local | PASS | Critical | Platform admin opened Tenant B rate limits, settings, projects, and team pages with 200 responses while holding no Tenant B membership. |
| CP-09 | Local | PASS | - | Playwright created, edited, and soft-deleted throwaway leaderboard id 61. Screenshot: `/private/tmp/ggscale-preprod-screenshots/24-cp-leaderboard-edit.png`. |
| CP-10/11 | Local | PASS | - | Rate limits, tenant settings, and project settings rendered; owner project invite quota and platform-admin tenant API/recipient overrides persisted. Current settings templates intentionally expose no public-joining control because players are linked by project admins rather than self-joining. Screenshots: `/private/tmp/ggscale-preprod-screenshots/25-cp-rate-limits.png`, `/private/tmp/ggscale-preprod-screenshots/26-cp-tenant-settings.png`, `/private/tmp/ggscale-preprod-screenshots/27-cp-project-settings.png`. |
| CP-12 | Local | PASS | - | Project invite-quota mutation without CSRF and with stale CSRF both returned 403; override rows remained unchanged. |
| CP-13 | Local | PASS | - | Throwaway CP user password-change flow denied wrong current password, accepted correct current password, revoked the old session, and allowed login with the new password. |
| PRIV-P03 | Local | PASS | Critical | Disabled throwaway player 927; its pre-disable access token was immediately rejected by `GET /profile` and `/server/player-sessions/verify`. |
| REG-06 / EDGE-11-verify-code | Local | PASS | - | Expired verify code for an unverified player returned 400; re-verifying an already verified player returned 400 and did not mint a new verify session. |
| EDGE-01-api-login-limiter | Local | PASS | - | Repeated bad API login attempts for throwaway `lockme-1784053690105@example.com` hit 429; correct password was also refused while limiter was active. Recovery probe after `Retry-After` returned 200. |
| EDGE-02-player-csrf | Local | PASS | Critical | Player friends mutation without `_csrf` and with stale `_csrf` both returned 403 and created no friend edge. CP CSRF negatives are covered by CP-12. |
| EDGE-03/09/10 | Local | PASS | - | Invalid email rejected; `max_players=0` created a default game session; joining the same session twice was idempotent; local `DELETE /healthz` returned 405 and dev CORS wildcarded preflight. |
| EDGE-07 | Local | PASS | - | Emoji, combining-mark, and RTL xuid persisted and re-read correctly; NUL/control-char xuid returned 400 without a 500. |
| EDGE-08 | Local | PASS | - | Storage pagination with 12 generated keys returned stable 5-item pages, a `next_cursor`, and no duplicates across the first two pages. |
| EDGE-05 | Local | PASS | Medium | Reconfirmed 2026-07-18, then fixed test-first: storage binds an open-ended `json.RawMessage` body, `text/plain` returns 415 without storing a row, normal `application/json` returns 200, and OpenAPI advertises JSON. Focused race-enabled integration and rebuilt live-server checks pass. |
| PROD-SMOKE / EDGE-09-prod | Production | BLOCKED | - | Production read-only smoke was not run because the plan still lacks the real `{{PROD_HOST}}` and expected CORS allowlist. No production writes or destructive tests should be run. |

### Branch follow-up run (revision `2757426`, started 2026-07-14 22:54 America/Chicago)

**Progress (updated 2026-07-18):** The BF-TIER-01 browser gap and BF-WS-04's exact live SIGKILL variant are closed. All automated request, approval, direct tier-change, downgrade-behavior, metrics, audit, invariant, regional-cap, exact-metric, and lease-recovery cases passed. Direct Playwright MCP cleared BF-TIER-01's visual browser step on the rebuilt Compose server. The shared local database was not reset; each direct tier probe was restored immediately.

**Continuation (2026-07-18):** BF-WS-01/02/03/05 and BF-AUTO-CLOSEOUT
now pass against the PostgreSQL grant design. BF-WS-04 passed after the holder
was killed with SIGKILL and its allocation became reusable within the lease
bound. The
browser-only BF-TIER-01 assertion now passes through the direct Playwright MCP.
EDGE-05 was fixed and retested as noted above.

| Test ID | Env | Result | Severity | Notes / evidence |
|---------|-----|--------|----------|------------------|
| BF-ENV-01 | Local compose | RESOLVED | - | The seven-hour-old app container did not contain the current migration CLI and tried to start a second server. Rebuilt only `ggscale-server` from revision `2757426`; health returned 200 and `ggscale-server migrate version` then reported `version=12 dirty=false`. Data volume was preserved. |
| BF-MIG-01 / BF-MIG-02 | Isolated PostgreSQL 17 | PASS | Critical | Added and ran `TestBranchFollowup_tier_class_migration_maps_and_round_trips`: legacy free/payg/premium mapped to 0/1/2; -1/4 were constrained; dependent project row survived; tier_3 down-mapped to premium and re-applied as tier_2; migration state remained version 12, clean. |
| BF-MIG-03 | Isolated PostgreSQL 17 | PASS | Critical | Added and ran `TestBranchFollowup_storage_usage_migration_backfills_live_canonical_bytes`: per-tenant totals matched live canonical `jsonb::text` bytes for nested/Unicode/empty values, ignored a soft-deleted row, and an empty tenant resolved to zero. |
| BF-MIG-04 | Isolated PostgreSQL 17 | PASS | - | Added and ran `TestBranchFollowup_maintain_grant_covers_existing_and_future_tables`: `ggscale_app` held MAINTAIN on existing `river_job` and a post-migration table, could ANALYZE, and remained unable to ALTER. |
| BF-MIG-05 | Isolated full-stack integration | PASS | Critical | Constraint accepted all compiled features and rejected an unknown feature. Matchmaker ticket creation changed 201 -> 403 after an explicit disabled grant/cache refresh, then returned to 201 after re-enable/cache refresh. |
| BF-MIG-06 | Isolated full-stack integration | PASS | Critical | Migration 0008 re-application/backfill remained idempotent; PA cross-tenant list/manage tests passed; disabling a PA removed its Casbin grant and enabling restored it. |
| BF-MIG-07 | Isolated PostgreSQL 17 + local compose | PASS | Critical | Runner force test proved dirty-state recovery without migration SQL; all CLI argument cases passed; rebuilt live binary reported `version=12 dirty=false`. No force was run against the shared database. |
| BF-REG-01 | Isolated full-stack integration | PASS | - | Block/unblock returned opaque 404 for both nonexistent and cross-tenant player IDs with no 500. |
| BF-CFG-01 | Isolated full-stack integration | PASS | Critical | With enforcement false, direct provisioning and public-signup approval persisted `enforce_quotas=false`; the existing signup happy-path test now asserts this default. Unenforced tier_0 project growth beyond three also passed. |
| BF-CFG-02 | Isolated full-stack integration | PASS | Critical | With enforcement true, direct provisioning and signup acceptance both persisted `enforce_quotas=true`. Direct provisioning retained exactly one starter project, key, owner membership, Casbin tenant-owner grouping, and creation audit. |
| BF-CFG-03 | Isolated full-stack integration | PASS | - | Starting servers with enforcement true and false did not rewrite two existing tenants with opposite enforcement values. |
| BF-PROJ-01 | Isolated full-stack integration | PASS | Critical | At two live tier_0 projects, 20 simultaneous creates produced one success and 19 user-facing project-limit conflicts; exactly three live rows remained. BF-OPS-01 captured `ggscale_quota_rejections_total{axis="projects"}` from the real handler. |
| BF-PROJ-02 | Isolated full-stack integration (`-race`) | PASS | Critical | The same 20-request race serialized at the tenant boundary: one insert committed and no overshoot occurred. |
| BF-PROJ-03 | Isolated full-stack integration | PASS | - | An unenforced tier_0 tenant grew to five projects and an enforced tier_3 tenant grew to 21 without quota rejection. |
| BF-PLYR-01 | Isolated full-stack integration | PASS | Critical | At 250,000 live tenant players, anonymous auth, email signup, new custom-token auth, and new invite acceptance all returned 403 without inserting a player; the rejected invite remained pending. BF-OPS-01 captured `ggscale_quota_rejections_total{axis="players"}` from the real handler. |
| BF-PLYR-02 | Isolated full-stack integration | PASS | Critical | At the cap, existing custom-token auth, refresh, profile access, and an invite linking a pre-existing player all succeeded without increasing the tenant count. |
| BF-PLYR-03 | Two-server isolated integration (`-race`) | PASS | Critical | At 249,999 tenant players, 20 synchronized mixed anonymous/custom-token creates across two routers yielded exactly one success and 19 quota denials; the live count stopped at 250,000. An allow-all test limiter kept the test focused on quota serialization. |
| BF-PLYR-04 | Isolated full-stack integration | PASS | - | The count was shared across two projects. Soft-deleting one player freed exactly one slot, and disabling enforcement subsequently allowed one row beyond the tier_0 limit. |
| BF-STOR-01 | Isolated full-stack integration (`-race`) | PASS | Critical | Create, grow, shrink, escaped Unicode/nested JSON replacement, delete, and repeated delete kept `tenant_storage_usage` equal to the canonical live `jsonb::text` sum, nonnegative, and finally zero. |
| BF-STOR-02 | Isolated full-stack integration (`-race`) | PASS | Critical | Invalid JSON (400), a value over 1 MiB (413), stale `If-Match` (412), and a trigger-injected database exception (500) left the object and counter unchanged; a valid conditional update advanced version and counter atomically. |
| BF-STOR-03 | Isolated full-stack integration (`-race`) | PASS | - | Injected tier_0 usage plus one real write landed exactly on 5 GiB and succeeded; a subsequent positive-byte write returned 403 without changing the counter. |
| BF-STOR-04 | Two-server isolated integration (`-race`) | PASS | Critical | Across 50 iterations, different keys, players, and projects were released concurrently with a trigger pause after the quota read. `GetTenantQuotaContext FOR UPDATE` serialized the tenant decision: one write committed, one returned 403, and usage never exceeded 5 GiB. |
| BF-STOR-05 | Isolated full-stack integration (`-race`) | PASS | - | At/over the limit, GET and list remained available, growth was denied, and shrinking plus delete succeeded and reduced usage. |
| BF-STOR-06 | Isolated full-stack integration (`-race`) | PASS | Critical | A full tenant did not affect a second tenant's write/counter; an unenforced tier_0 tenant grew beyond 5 GiB while the independent per-value 1 MiB rejection remained active. |
| BF-STOR-07 | Isolated warning-worker integration (`-race`) | PASS | - | Sequential sweeps covered below 80, 80, unchanged 80, 100, down to 80, down to 0, and re-crossing 80. Emails occurred only on upward crossings and stored thresholds followed downward transitions. |
| BF-STOR-08 | Isolated warning-worker integration (`-race`) | PASS | - | Only verified current owner/admin recipients were included; verified member, unverified admin, removed admin, cross-tenant owner, and an unenforced tenant were excluded. Unchanged thresholds did not redeliver. |
| BF-STOR-09 | Isolated warning-worker integration (`-race`) | PASS | Critical | An injected first-send error left the threshold at 0; the next sweep retried, delivered exactly once, and only then stored threshold 80. |
| BF-WS-01 | Two HTTP servers + PostgreSQL grants (`-race`) | PASS | Critical | Live same-region storm admitted exactly 4 of 100 across two east processes; 96 returned 503. Closing one socket allowed one replacement. |
| BF-WS-02 | PostgreSQL 17 grant integration (`-race`) | PASS | Critical | App-role integration tests and live east/west processes proved the regional hard wall and independent regional envelopes. |
| BF-WS-03 | PostgreSQL grant/burst tests (`-race`) | PASS | Critical | Race-enabled PostgreSQL tests verify sustained/ceiling behavior plus burst-budget charge/refill under the grant model. |
| BF-WS-04 | Lease, renewal, and process-loss integration (`-race`) | PASS | Critical | Batched renewal and clean release passed. Port 8081 then held four sockets with 38 seconds on its grant; after SIGKILL of the verified PID, port 8082 rejected immediately and admitted after roughly 30 seconds, within the 45-second lease bound. |
| BF-WS-05 | Realtime handler and outage tests (`-race`) | PASS | Critical | Race-enabled handler/cap tests verify player-first ordering, tenant rejection, bounded emergency admission, and rejection after exhaustion. |
| BF-WS-06 | Realtime/ratelimit unit tests (`-race`) | PASS | - | tier_0 through tier_3 resolved to 5k/10k, 20k/40k, 50k/100k, and 50k/100k; unknown tier fell back to tier_0; a positive environment override set sustained=ceiling. |
| BF-REQ-01 | Isolated full-stack integration (`-race`) | PASS | Critical | Owner/admin upward requests persisted the requested tier, note, requester, and pending state; tenant history and the PA queue rendered those fields. Current, lower, out-of-range, and nonnumeric targets inserted no row. |
| BF-REQ-02 | Isolated full-stack integration (`-race`) | PASS | - | Environment-enabled relay and dedicated-server requests covered approve and deny, grant metadata, reason email, and recipient. Disabled/unknown/already-enabled forged requests inserted no pending row. |
| BF-REQ-03 | Isolated full-stack integration (`-race`) | PASS | Critical | Owner/admin submissions succeeded; member, unrelated user, missing/stale CSRF, and tenant-owner PA-queue access were denied. Twenty concurrent duplicate submissions left exactly one pending request. |
| BF-REQ-04 | Isolated full-stack integration (`-race`) | PASS | Critical | approve/deny, approve/approve, and deny/deny races each produced one terminal transition, one side effect, one platform audit, and one email. |
| BF-REQ-05 | Isolated full-stack integration (`-race`) | PASS | Critical | After a tier_0 -> tier_1 request was made stale by a direct tier_2 change, approval left the tenant at tier_2 and the request pending, with no approval audit or email. |
| BF-TIER-01 | Isolated integration + rebuilt local HTTP form + direct Playwright MCP | PASS | Critical | Existing isolated form/persistence/audit coverage passed. On 2026-07-18, `admin@demo.ggscale` used the rendered selector for tenant 9 to change tier_0 -> tier_1, confirmed the tier_1 display and 1000/2000 defaults after reload, then restored tier_1 -> tier_0 and confirmed the 150/300 defaults. Control-panel login/logout and the successful page produced no console errors. Screenshot: `/private/tmp/ggscale-prodready-browser-20260718/prodready-bf-tier-01-restored.png`. |
| BF-TIER-02 | Isolated full-stack integration (`-race`) | PASS | Critical | Owner/admin/member/unrelated posts and missing/stale CSRF were denied; -1, 4, text, missing/deleted tenant, and same-tier paths did not mutate or emit an audit. |
| BF-TIER-03 | Isolated full-stack integration (`-race`) | PASS | Critical | Thirty concurrent changes from two PAs produced a serialized audit chain: every old tier matched the prior committed new tier, and actor, target, and direction were correct. |
| BF-TIER-04 | Isolated full-stack integration (`-race`) | PASS | - | An enforced tier_2 tenant with 4 projects, 250,001 players, and storage above 5 GiB was downgraded. No data or session was lost; reads and shrink worked; growth on all axes was blocked at tier_0 and restored immediately at tier_2. |
| BF-OPS-01 | Full-stack integration + ratelimit unit (`-race`) | PASS | - | Real project/player/storage handler rejections emitted exactly one metric for each axis. Deterministic CCU decisions emitted ceiling and budget metrics. Series exposed only low-cardinality `axis`/`reason` labels and no tenant/player IDs. |
| BF-OPS-02 | Isolated integration + rebuilt local logs | PASS | Critical | Approve, deny, and tier-change audits had the authenticated PA and exact request/tenant targets with one event per terminal action. Payload scans found none of the test API key, password, custom-token secret, CSRF values, or session cookies. The rebuilt server emitted no success-path payload logs, and a 20-minute log scan found no credential/token matches. |
| BF-OPS-03 | Isolated PostgreSQL 17 | PASS | Critical | After normal feature/request/tier/storage activity, invariant queries returned zero finite project/player quota violations, zero canonical storage-meter mismatches, and zero duplicate pending request groups. Intentionally injected boundary fixtures lived in separate disposable databases. |
| BF-AUTO-CLOSEOUT | Local + Docker (`-race`) | PASS | Critical | On the final 2026-07-18 working tree based on `26909467c0ba21956b339ba3290ad48387428e71`, `make lint`, `go test -race ./...`, `INTEGRATION_PARALLEL=2 make test-integration`, `make e2e`, and OpenAPI regeneration/diff passed. The new in-place cap-change regression also passed ten race-enabled repetitions in 26.286 seconds. The Compose stack rebuilt from the retained volume with migration 14 clean and all services healthy. Commit the grant regression test and result updates, then bind the evidence to that immutable revision. |

---

## 21. Appendix — quick reference

**Bases:** API `{{BASE}}` · CP `{{BASE}}/control-panel` · Players `{{BASE}}/players` · Mailpit `http://localhost:8025` (`/api/v1/messages`).

**Headers:** `Authorization: Bearer <key>` · `X-Session-Token: <jwt>` · CP CSRF `X-CSRF-Token`/`_csrf` (session cookie `ggscale_control_panel_session`) · player CSRF cookie `ggscale_csrf` + `_csrf` (session cookie `ggscale_player_session`).

**Key types:** `publishable` (client-embeddable) vs `secret` (server-only: score submit, session verify, fleet heartbeat). **Scopes:** `matchmaker` (default-on), `fleet`, `p2p_relay` (`fleet`/`p2p_relay` also feature-gated; out of scope except PRIV‑K02/K03).

**TTLs:** access `15m` · refresh `30d` (rotates) · verify code `15m` (5/code, 20 lifetime, 24h lockout) · game invite ~`5m` · player/link invite `3d` · team/tenant-signup invite `7d` · CP session `12h` (slides 1h) · player session `30d`.

**Magic-link accept URLs:** team → `{{CP}}/invite/accept?code=` · tenant signup → `{{CP}}/request-access/accept?code=` · player/link → `{{PLAYERS}}/p/{projectID}/invite/accept?code=`.

**Dashboard roles → capability:**
| | owner | admin | member | +fleet_operator |
|---|---|---|---|---|
| manage tenant | ✓ | ✗ | ✗ | ✗ |
| manage projects | ✓ | ✓ | read | ✗ |
| api_key publishable | ✓ | ✓ | ✗ | ✗ |
| api_key secret | ✓ | ✗ | ✗ | ✗ |
| team / member-role grants | ✓ | ✗ | ✗ | ✗ |
| players manage (incl. link/disable/ban) | ✓ | ✓ | read | ✗ |
| leaderboards manage | ✓ | ✓ | read | ✗ |
| project invite quotas | ✓ | ✓ | ✗ | ✗ |
| API ceiling / recipient invite override | PA only | PA only | ✗ | ✗ |
| feature_grants / tenant-signup admin | PA only | PA only | ✗ | ✗ |
| fleet / allocation | ✗ | ✗ | read | ✓ |

**Deny outcomes:** `401` unauth · `403` forbidden · `404` hidden/absent (acceptable deny). A `200` with another tenant's data is always critical.

**Do NOT hand-insert** keys/memberships/casbin rows — create via CP so Casbin `g`-rows are written (§3).

---

## 22. Branch follow-up integration plan

This is the release-gate plan for the quota and service-class work on `ws-connection-enforcement`, including the fixes made during review and the immediately preceding migration/RBAC commits. It is intentionally narrower and more adversarial than the general suite above.

**Local only.** Every case in this section mutates disposable data or infrastructure. Do not run any `BF-*` case against Production. Start from a fresh database, retain logs and Mailpit messages for evidence, and reset again when finished.

### 22.1 Scope and release gate

| Area | Behavior under test | Cases |
|------|---------------------|-------|
| Migrations and operator CLI | River maintenance grant, matchmaker constraint, numeric tier migration, storage backfill, platform-admin policy, `migrate version/force` | BF-MIG-* |
| Enforcement opt-in | `QUOTAS_ENFORCE_NEW_TENANTS`, existing-tenant compatibility | BF-CFG-* |
| Project/player quotas | all creation paths, no-growth paths, tenant-wide serialization | BF-PROJ-*, BF-PLYR-* |
| Storage | transactional metering, tenant-wide quota serialization, warning delivery/retry | BF-STOR-* |
| Realtime | tier envelopes, regional PostgreSQL grants, local fast path, bounded outage behavior | BF-WS-* |
| Change management | tenant upgrade/feature requests, approval races, direct admin downgrade | BF-REQ-*, BF-TIER-* |
| Recent regressions | opaque friend denial from the commit preceding platform-admin RBAC | BF-REG-* |
| Operations | audit trail, metrics, failure behavior | BF-OPS-* |

**Release gate:** all `[critical]` cases must pass, the final database invariants must hold, and `make lint`, `make test`, and `make test-integration` must pass on the same revision. Any quota bypass, count overshoot, stale-request downgrade, cross-tenant mutation, or distributed CCU overshoot is a release blocker.

### 22.2 Run order and fixtures

Run in this order so destructive fixtures do not contaminate later assertions:

1. BF-MIG on an isolated temporary database.
2. Reset, then BF-CFG and BF-PROJ using control-panel-created tenants.
3. Reset, then BF-PLYR using a dedicated bulk-count tenant.
4. Reset, then BF-STOR with Mailpit available; stop it only for BF-STOR-09.
5. Reset, then BF-WS with at least two server processes sharing PostgreSQL and
   the same `APP_REGION`; use a second region value for isolation cases.
6. Reset, then BF-REQ, BF-TIER, and BF-OPS.
7. Run the automated checks and archive the results.

Create these fixtures through the UI unless a case explicitly calls for database fault/boundary injection:

| Handle | Fixture |
|--------|---------|
| `{{Q_TENANT}}` | disposable tier_0 tenant, `enforce_quotas=true`, verified owner/admin/member |
| `{{U_TENANT}}` | disposable tier_0 tenant, `enforce_quotas=false` |
| `{{Q_PROJECT}}` | starter project in `{{Q_TENANT}}` with publishable and secret keys |
| `{{Q_PLAYER}}` | verified existing player with valid access/refresh tokens captured before filling the player quota |
| `{{Q_CUSTOM_EXISTING}}` | existing custom-token external ID; tenant custom-token secret configured |
| `{{Q_INVITE_EXISTING}}` | pending invite that targets an already-existing project player |
| `{{Q_INVITE_NEW}}` | pending invite that would create a new project player |
| `{{PA2}}` | second platform admin for concurrent approval/tier-change tests |

Use the actual class ladder; do not patch constants for this pass:

| Class | Projects | Registered players | Storage | Sustained/ceiling CCU |
|-------|----------|--------------------|---------|-----------------------|
| tier_0 | 3 | 250,000 | 5 GiB | 5,000 / 10,000 |
| tier_1 | 10 | 1,000,000 | 25 GiB | 20,000 / 40,000 |
| tier_2 | 20 | 5,000,000 | 100 GiB | 50,000 / 100,000 |
| tier_3 | unlimited | unlimited | 500 GiB | 50,000 / 100,000 |

For the 250,000-player boundary, bulk-insert inert `project_players` only in the disposable tenant. Prefix every external ID with the run ID, record the inserted ID range, and reset the database after BF-PLYR. Direct inserts are allowed for this boundary fixture only; continue to create API keys, memberships, invitations, and Casbin rows through application flows.

For storage boundaries, inject `tenant_storage_usage.total_bytes` near 5 GiB instead of allocating gigabytes. Cases that validate counter accuracy must use real API writes and compare the counter with the live-object sum before any boundary injection.

### 22.3 Migration and configuration cases

**Coverage index**
- [x] BF-MIG-01 numeric tier data migration `[critical]`
- [x] BF-MIG-02 migration constraints and reverse mapping
- [x] BF-MIG-03 storage usage backfill `[critical]`
- [x] BF-MIG-04 River MAINTAIN grant
- [x] BF-MIG-05 matchmaker feature-grant constraint `[critical]`
- [x] BF-MIG-06 platform-admin Casbin policy `[critical]`
- [x] BF-MIG-07 migration version/force command `[critical]`
- [x] BF-REG-01 nonexistent friend block/unblock is opaque
- [x] BF-CFG-01 enforcement default preserves self-host behavior `[critical]`
- [x] BF-CFG-02 new-tenant enforcement through both provisioning paths `[critical]`
- [x] BF-CFG-03 restarts do not rewrite existing tenants

**BF-MIG-01 - numeric tier data migration** `[critical]`
Pre: Isolated database at migration 0008 with tenants on `free`, `payg`, and `premium`.
Do: Apply 0009 through 0012.
Expect: old values map to 0, 1, and 2; the column is `smallint`; new tenants default to 0; values below 0 or above 3 are rejected. No tenant, project, key, membership, or Casbin row is lost.

**BF-MIG-02 - reverse mapping and clean re-apply**
Do: On a disposable database, migrate down through 0009, inspect the restored string values, then migrate back up. Include a tier_3 row before the down migration.
Expect: down migration follows its documented lossy tier_3 mapping, schema constraints are valid in both directions, and the second up reaches version 12 without dirty state.

**BF-MIG-03 - storage usage backfill** `[critical]`
Pre: Before 0011, create live, soft-deleted, empty, Unicode, and nested-JSON storage objects in two tenants.
Do: Apply 0011. Compare `tenant_storage_usage.total_bytes` with `SUM(octet_length(value::text))` over live objects per tenant.
Expect: totals match canonical `jsonb::text` byte lengths; soft-deleted rows are excluded; tenants with no live objects behave as zero; no tenant's bytes are attributed to another tenant.

**BF-MIG-04 - River maintenance privilege**
Do: After 0006, `SET ROLE ggscale_app` and run representative `ANALYZE`/maintenance privilege checks on an existing River table and a table created after the default-privilege change.
Expect: `ggscale_app` has PostgreSQL 17 `MAINTAIN` on existing and future app-schema tables, River's reindex/maintenance work no longer logs permission errors, and the role does not gain ownership or unrelated DDL/data privileges.

**BF-MIG-05 - matchmaker grant constraint** `[critical]`
Do: After 0007, create an explicit tenant-level `feature_grants(feature='matchmaker', enabled=false)` row through the PA path/authorized transaction; wait through the authorizer cache window and try to create a ticket with a matchmaker-scoped key. Re-enable it and retry.
Expect: both grant states persist without a constraint error; disabled returns 403 even when the key has scope; re-enabled restores ticket creation. Other invalid feature names remain constrained. This is the focused rerun of REG-09/PRIV-K03.

**BF-MIG-06 - platform-admin Casbin policy** `[critical]`
Do: Apply 0008 twice in the isolated migration harness, log in as PA, a tenant owner, and a normal user, and probe `/admin/*` plus cross-tenant CP pages.
Expect: PA is authorized through the platform-admin policy and may manage any tenant; tenant users remain denied; repeated application creates no duplicate or broader policy. Disabling a platform admin still blocks login.

**BF-MIG-07 - migration version and force command** `[critical]`
Pre: Disposable database only. Never exercise `force` on a healthy shared or production database.
Do: Run `ggscale-server migrate version`; deliberately mark the disposable migration state dirty; run `ggscale-server migrate force <last-good-version>`; run `version` again; then run normal migrations. Also try missing, negative, non-numeric, and extra arguments.
Expect: version reports both version and dirty flag; force changes migration metadata without executing migration SQL; dirty clears only at the requested version; normal migration recovery succeeds; invalid invocations exit non-zero with usage and leave state unchanged.

**BF-REG-01 - nonexistent friend block/unblock denial**
Do: As an authenticated linked player, POST block and unblock for a nonexistent player/account ID and an ID outside the caller's visible relationship context.
Expect: both operations return the same opaque 404 used by friend requests, never 500, expose no existence/tenant detail, and create/delete no relationship row. This is the focused rerun of REG-10/ISO-06.

**BF-CFG-01 - enforcement default** `[critical]`
Pre: Start with `QUOTAS_ENFORCE_NEW_TENANTS` unset/false.
Do: Create one tenant through CP and approve one public tenant-signup request.
Expect: both tenants have `enforce_quotas=false`; class-based project/player/storage growth is not blocked. The always-on HTTP rate limiter and realtime connection cap remain enabled.

**BF-CFG-02 - enforced provisioning paths** `[critical]`
Pre: Restart with `QUOTAS_ENFORCE_NEW_TENANTS=true`.
Do: Repeat direct CP tenant creation and tenant-signup approval.
Expect: both new tenants atomically persist `enforce_quotas=true`; each still has its starter project, key, owner membership, Casbin grouping, and creation audit. A failure while setting enforcement rolls back the whole provisioning transaction.

**BF-CFG-03 - existing tenant compatibility**
Do: Toggle the environment setting across two restarts without creating tenants.
Expect: no existing tenant's `enforce_quotas` value changes. Direct tier changes also do not toggle enforcement.

### 22.4 Project and player quota cases

**Coverage index**
- [ ] BF-PROJ-01 exact project boundary `[critical]`
- [x] BF-PROJ-02 concurrent project creates serialize `[critical]`
- [x] BF-PROJ-03 unenforced and unlimited behavior
- [ ] BF-PLYR-01 every new-player path is capped `[critical]`
- [x] BF-PLYR-02 existing-player paths remain available `[critical]`
- [x] BF-PLYR-03 mixed concurrent player creates serialize `[critical]`
- [x] BF-PLYR-04 quota is tenant-wide and soft-delete aware

**BF-PROJ-01 - exact boundary** `[critical]`
Pre: `{{Q_TENANT}}` tier_0 has exactly two live projects.
Do: Create a third project, then attempt a fourth through the CP.
Expect: the third succeeds; the fourth returns a user-facing quota error; exactly three live rows remain; `ggscale_quota_rejections_total{axis="projects"}` increments once. Existing projects, keys, and reads continue to work.

**BF-PROJ-02 - concurrent create race** `[critical]`
Pre: tier_0 enforced tenant with two live projects.
Do: Submit at least 20 simultaneous valid project-create POSTs with distinct names and valid CSRF tokens.
Expect: exactly one succeeds, the rest receive quota responses, and the committed live count is exactly three. Repeat several times after reset to expose timing-sensitive overshoot.

**BF-PROJ-03 - opt-out and unlimited class**
Do: Attempt project growth beyond three on `{{U_TENANT}}`; then set an enforced disposable tenant to tier_3 and create more than 20 projects.
Expect: neither is blocked by the class project quota. Other validation, duplicate-name checks, authorization, and rate limiting still apply.

**BF-PLYR-01 - cap every creation path** `[critical]`
Pre: `{{Q_TENANT}}` tier_0 has exactly 250,000 live players across at least two projects.
Do: Attempt each operation with a genuinely new identity:

- `POST /v1/auth/anonymous`
- `POST /v1/auth/signup`
- `POST /v1/auth/custom-token` with a new external ID
- accept `{{Q_INVITE_NEW}}` in the player-site UI

Expect: every path is denied with a stable 403/user-facing quota message; no player, session, verification state, accepted invitation, or audit row implying successful creation is committed. The total remains 250,000 and the player rejection metric increments for each denied growth attempt.

**BF-PLYR-02 - no-growth paths at the cap** `[critical]`
Do: At the same cap, log in and refresh `{{Q_PLAYER}}`; authenticate `{{Q_CUSTOM_EXISTING}}`; accept `{{Q_INVITE_EXISTING}}` where the target project-player row already exists; read/update an existing profile.
Expect: all succeed because none increases the registered-player count. The invite binds/links the intended existing row and does not create a second row.

**BF-PLYR-03 - mixed concurrent creation race** `[critical]`
Pre: exactly 249,999 live players.
Do: Simultaneously issue a large set of anonymous-auth and distinct custom-token requests; include signup and invite acceptance where the harness supports them.
Expect: exactly one new `project_players` row commits across the entire tenant, independent of project; all other growth attempts receive quota denials; no request returns 500. Repeat with requests split across two server instances.

**BF-PLYR-04 - tenant-wide count and soft delete**
Do: Distribute the boundary rows over two projects, verify a third project's create path is still blocked, then soft-delete one player and retry.
Expect: quota counts all live players in the tenant, not just the pinned project; the single freed slot admits exactly one new player. An unenforced tenant can grow past 250,000.

### 22.5 Storage metering, quota, and warning cases

**Coverage index**
- [x] BF-STOR-01 counter lifecycle `[critical]`
- [x] BF-STOR-02 failed writes do not change usage `[critical]`
- [x] BF-STOR-03 exact quota boundary
- [x] BF-STOR-04 concurrent different-key writes serialize `[critical]`
- [x] BF-STOR-05 over-limit recovery operations
- [x] BF-STOR-06 tenant isolation and unenforced behavior `[critical]`
- [x] BF-STOR-07 80/100 warning transitions
- [x] BF-STOR-08 recipients and deduplication
- [x] BF-STOR-09 failed delivery is retried `[critical]`

**BF-STOR-01 - transactional counter lifecycle** `[critical]`
Do: Create an object, grow it, shrink it, replace it with Unicode/nested JSON, then delete it. After each request compare `tenant_storage_usage.total_bytes` to the sum of `octet_length(value::text)` for every live object in the tenant.
Expect: the counter changes by the canonical JSON byte delta, never goes negative, and returns to its prior value after delete. Repeated delete is idempotent.

**BF-STOR-02 - rollback paths** `[critical]`
Do: Cause invalid JSON, per-value-size rejection, stale `If-Match`, and a database-side write failure.
Expect: neither the object nor tenant counter changes. A successful conditional write updates object/version/counter in one commit.

**BF-STOR-03 - exact boundary**
Pre: Inject tier_0 usage to just below 5 GiB.
Do: Write a value that lands exactly on the limit, then one that exceeds it by one canonical JSON byte.
Expect: exact-limit growth succeeds; over-limit growth returns 403 with the limit and does not change the object or counter.

**BF-STOR-04 - concurrent different-key race** `[critical]`
Pre: Set total usage so only one of two new values can fit. Use different keys, players, and projects in the same tenant to avoid object-lock serialization.
Do: Release both PUTs from a barrier at the same instant; repeat at least 50 times from two server instances.
Expect: exactly one write commits per run, the other receives quota denial, and total usage never exceeds 5 GiB. This proves the quota decision is serialized tenant-wide, not merely per object key.

**BF-STOR-05 - recovery while full/over limit**
Do: At and above the limit, GET/list objects, delete an object, and overwrite an object with a smaller value; also attempt a growing overwrite.
Expect: reads, deletes, and shrinking writes remain available; growing writes are denied until enough space is freed; the next permitted growth uses the newly freed bytes.

**BF-STOR-06 - isolation and opt-out** `[critical]`
Do: Fill `{{Q_TENANT}}`, then write in Tenant B and `{{U_TENANT}}`. Compare all three counters.
Expect: one tenant's usage never blocks or changes another tenant; unenforced storage is not class-capped but still observes `STORAGE_MAX_VALUE_BYTES` and per-project overrides.

**BF-STOR-07 - threshold transitions**
Do: Drive an enforced tenant through below-80%, 80%, 100%, below-100%, below-80%, and back above 80%. Trigger/restart the leader-elected storage warning job after each transition.
Expect: one email on first crossing 80, one on crossing 100, no repeat at an unchanged threshold, the stored threshold follows downward transitions without email, and a later upward re-crossing sends again.

**BF-STOR-08 - warning recipients**
Do: Give the tenant a verified owner, verified admin, verified member, unverified admin, and removed admin; cross 80%.
Expect: the message is addressed only to verified current owner/admin users, with one delivery per threshold and no cross-tenant recipients. Unenforced tenants are absent from the sweep.

**BF-STOR-09 - delivery failure retry** `[critical]`
Pre: Usage is newly above 80 and `last_notified_threshold=0`. Stop Mailpit/SMTP before triggering the sweep.
Do: Run the sweep once with SMTP unavailable, inspect the threshold, restore SMTP, and trigger the next sweep.
Expect: failed delivery is logged but the threshold remains 0; the next sweep retries, Mailpit receives the warning, and only then does the threshold become 80. A transient mail failure must not suppress the warning permanently.

### 22.6 Realtime regional-cap cases

The production tier_0 envelope is too large for a deterministic race test. Use
two app processes with distinct holder IDs, a shared write-primary PostgreSQL
database, the same `APP_REGION`, and `REALTIME_MAX_PER_TENANT=4`. A third
process with another `APP_REGION` proves isolation. Unit/integration harnesses
may use short leases and burst budgets; do not change production constants for
the browser/API run.

**Coverage index**
- [ ] BF-WS-01 regional hard override across processes `[critical]`
- [ ] BF-WS-02 region isolation `[critical]`
- [ ] BF-WS-03 clean release and crashed-process lease recovery `[critical]`
- [ ] BF-WS-04 burst budget charges and refills `[critical]`
- [ ] BF-WS-05 batched renewal and local fast path
- [ ] BF-WS-06 endpoint ordering and bounded grant failure `[critical]`
- [ ] BF-WS-07 tier envelope resolution

**BF-WS-01 - shared hard cap** `[critical]`
Pre: Two app instances share the write primary, use the same `APP_REGION`, and
set `REALTIME_MAX_PER_TENANT=4`.
Do: Open at least 100 simultaneous valid WebSocket dials for many players in one tenant, balanced across both instances.
Expect: exactly four are admitted globally; all other dials receive 503 with `Retry-After: 5`; closing one admitted socket makes exactly one slot available. A second tenant has its own four slots.

**BF-WS-02 - region isolation** `[critical]`
Do: Keep four sockets admitted in `us-east`, then dial the same tenant through
an otherwise identical process configured as `us-west`.
Expect: west has its own four-slot envelope. Rows are separated by
`(tenant_id, region)`, while two processes inside either region share one wall.

**BF-WS-03 - release and process-loss recovery** `[critical]`
Do: Close the last socket on one holder and verify another holder can reuse its
capacity immediately. Repeat by killing the first process without graceful
shutdown.
Expect: clean release is immediate; crashed-process allocation remains reserved
for at most the 45-second lease and is then reclaimed. Existing sockets on a
live process are never dropped to repair stale accounting.

**BF-WS-04 - burst accounting** `[critical]`
Do: With a short test budget, admit above sustained, hold, release, and try to
return above sustained. Then remain at/below sustained for the refill window.
Expect: time above sustained drains budget, connect/disconnect loops cannot
reset it, and refill occurs only at/below sustained and never above the maximum.

**BF-WS-05 - batch renewal and local fast path**
Do: Hold sockets for many tenants for at least three renewal intervals while
recording database queries and socket heartbeats.
Expect: ordinary admissions consume an existing process-local grant without a
query. Each process renews all active tenant grants in one batched transaction per
15-second interval. Socket heartbeats do not issue tenant-cap database calls.

**BF-WS-06 - endpoint ordering and grant failure** `[critical]`
Do: Exhaust a player's process-local cap before tenant capacity, then exhaust
tenant capacity with multiple players. Inject a grant-store failure after
authentication and continue dialing one tenant beyond its emergency allowance.
Expect: player excess gets the player-specific 503 without reserving tenant
capacity. During the outage, at most `min(64, max(8, sustained/1000))` extra
sockets are admitted per process and tenant; later dials receive 503. Existing
sockets remain connected. Recovery resumes normal grant synchronization.

**BF-WS-07 - class and override resolution**
Do: For keys issued to tier_0 through tier_3 tenants, assert the resolved sustained/ceiling values. Then set an environment override.
Expect: class values match the table in 22.2; unknown classes fail closed to tier_0; a positive environment override produces a fixed hard cap with sustained=ceiling and no burst.

### 22.7 Upgrade requests and platform-admin downgrade cases

**Coverage index**
- [x] BF-REQ-01 tenant submits upward-only tier request `[critical]`
- [x] BF-REQ-02 feature request lifecycle
- [x] BF-REQ-03 authorization, CSRF, and pending uniqueness `[critical]`
- [x] BF-REQ-04 approval/denial concurrency `[critical]`
- [x] BF-REQ-05 stale upgrade cannot downgrade `[critical]`
- [x] BF-TIER-01 platform-admin direct downgrade UI `[critical]`
- [x] BF-TIER-02 direct tier authorization and validation `[critical]`
- [x] BF-TIER-03 concurrent tier-change audit chain `[critical]`
- [x] BF-TIER-04 downgrade effects are non-destructive

**BF-REQ-01 - upward-only request** `[critical]`
Do: As tenant owner/admin, open Tenant Settings, submit tier_0 -> tier_1, and inspect the tenant history and PA queue. Forge current/lower/out-of-range tier values.
Expect: only a strictly higher 0..3 target is accepted; history and PA queue show tenant, current tier, target, note, requester, and pending status; forged non-upgrades create no row.

**BF-REQ-02 - feature lifecycle**
Do: Request each environment-enabled feature, approve one and deny one with a reason. Try a disabled, unknown, and already-enabled feature.
Expect: only requestable enabled features are offered/accepted; approval upserts one tenant-level enabled grant with approver/reason; denial changes no grant; verified tenant admins receive the appropriate decision email.

**BF-REQ-03 - authorization, CSRF, and duplicate pending** `[critical]`
Do: Submit as owner/admin, member, unrelated tenant user, unauthenticated caller, and with missing/stale CSRF. Submit two simultaneous pending requests of the same kind/feature. Probe the PA queue as a tenant owner.
Expect: only authorized tenant managers can submit for their own tenant; only PAs can list/review globally; denials mutate nothing; exactly one duplicate request remains pending and the loser gets a friendly response.

**BF-REQ-04 - review race** `[critical]`
Do: Have PA and `{{PA2}}` concurrently approve/deny the same pending request. Repeat approve/approve and deny/deny.
Expect: exactly one terminal transition, one applied side effect, one platform audit event, and one decision email. The loser sees "already handled" and cannot overwrite reviewer, reason, or status.

**BF-REQ-05 - stale request revalidation** `[critical]`
Pre: Submit a tier_0 -> tier_1 request. Before approval, directly raise the tenant to tier_2.
Do: Approve the stale tier_1 request.
Expect: approval is refused in the same transaction; tenant remains tier_2; request remains pending; no approval audit/email is emitted. Repeat with the current tier changed concurrently while approval is in flight.

**BF-TIER-01 - direct downgrade form** `[critical]`
Do: Log in as PA, open any tenant's Settings, confirm the tier selector is present, change tier_2 -> tier_0, and reload.
Expect: the UI persists and displays tier_0; one `control_panel.tenant.tier_change` platform audit row records actor, tenant, old=2, new=0, direction=`downgrade`. Upgrade requests remain a separate tenant-facing, upward-only form.

**BF-TIER-02 - direct-change authorization and validation** `[critical]`
Do: Forge the tier POST as owner/admin/member/unrelated user, without CSRF, and with -1, 4, text, and a missing/deleted tenant. Submit the current tier as PA.
Expect: non-PAs and invalid CSRF are denied; invalid tiers do not write; missing tenant is 404; same-tier submission is a no-op with no audit row.

**BF-TIER-03 - atomic audit under concurrent admins** `[critical]`
Do: From a known starting tier, have PA and `{{PA2}}` submit different tier changes concurrently many times.
Expect: each committed change's `old_tier` equals the immediately preceding committed value, its `new_tier` matches the row after that transaction, and actor/direction are correct. No audit row reports a stale old value.

**BF-TIER-04 - downgrade behavior**
Do: Give an enforced tier_2 tenant resource counts above tier_0 limits, downgrade it to tier_0, and exercise reads, existing-player login, delete/shrink, and new growth. Then upgrade it again.
Expect: downgrade does not delete or disable existing projects/players/storage; recovery operations remain available; new project/player/storage growth is blocked by tier_0 immediately; upgrading raises limits immediately. `enforce_quotas` remains unchanged.

### 22.8 Operational evidence and final invariants

**BF-OPS-01 - metrics**
Cause one project, player, and storage rejection plus CCU ceiling and budget rejections. Expect `ggscale_quota_rejections_total` labels `projects`, `players`, and `storage`, and `ggscale_connection_cap_rejections_total` labels `ceiling` and `budget`, with no tenant/player IDs in metric labels.

**BF-OPS-02 - audit and log hygiene** `[critical]`
Inspect platform audit rows for request approvals/denials and direct tier changes. Expect the authenticated actor and exact target in each record, no duplicate event after a lost race, and no API keys, custom-token secrets, session tokens, CSRF tokens, passwords, or full invite codes in logs/audit payloads.

**BF-OPS-03 - final database invariants** `[critical]`
Before reset, assert:

```sql
-- No enforced tenant exceeds its finite live project/player limit as a result
-- of a BF test. Compare counts to the class table in section 22.2.
SELECT t.id, t.tier,
       (SELECT count(*) FROM projects p
        WHERE p.tenant_id = t.id AND p.deleted_at IS NULL) AS projects,
       (SELECT count(*) FROM project_players pp
        WHERE pp.tenant_id = t.id AND pp.deleted_at IS NULL) AS players
FROM tenants t
WHERE t.enforce_quotas AND t.deleted_at IS NULL;

-- Metered bytes equal live canonical JSON bytes for non-injected fixtures.
SELECT t.id,
       COALESCE(u.total_bytes, 0) AS metered,
       COALESCE(SUM(octet_length(o.value::text))
                FILTER (WHERE o.deleted_at IS NULL), 0) AS actual
FROM tenants t
LEFT JOIN tenant_storage_usage u ON u.tenant_id = t.id
LEFT JOIN storage_objects o ON o.tenant_id = t.id
GROUP BY t.id, u.total_bytes;

-- At most one request of a kind/feature is pending per tenant.
SELECT tenant_id, kind, COALESCE(feature, ''), count(*)
FROM tenant_change_requests
WHERE status = 'pending'
GROUP BY tenant_id, kind, COALESCE(feature, '')
HAVING count(*) > 1;
```

The duplicate-pending query must return no rows. The metering query must match for every tenant not intentionally boundary-injected; restore/recompute injected counters before evaluating it. Investigate any count above a finite limit by correlating timestamps with the BF concurrency run rather than treating pre-existing over-limit fixtures as a product failure.

**Automated closeout:**

```bash
make lint
make test                  # go test -race ./...
make test-integration      # go test -race -tags=integration -parallel=8 ./...
```

Record each `BF-*` result in section 20 with revision, environment/config
overrides, server count, `APP_REGION`, request concurrency, actual committed
counts, relevant metric deltas, Mailpit message IDs, and audit row IDs. Save
browser screenshots for the request queue, direct tier selector, downgrade
confirmation, and tenant settings usage display.
