# ggscale tier & pricing model

Decision record, 2026-07-11; **class bundles amended 2026-07-14** (new ladder,
CCU burst, storage enforcement pulled in-scope — implementation plan:
`docs/temp/tier-rework.md`); **re-amended 2026-07-18** (ladder reshaped per
the competitive/infra research in `docs/recent-pricing-tier.md`: smaller
tier_0/1/2 caps, relay-session allowance axis added; tier_3 unchanged). Supersedes the tier portion of
`docs/archive/pricing-strategy.md` (which remains the reference for competitor
market research and support/services pricing). This document defines how
tenant limits are structured in the OSS server and how a commercial layer maps
prices onto them. It is internal planning, not public pricing copy.

## Core idea

Tenants are placed into price-neutral **capacity classes** (buckets of limits).
The OSS server ships the *mechanism* (classes, enforcement, feature gates). A
separate commercial layer ships the *policy* (which class costs what). No dollar
amounts, plan names, or billing logic live in the OSS repo.

## Three independent primitives

Each already has a home in the codebase.

| Primitive | Mechanism | Governs |
|---|---|---|
| Capacity **class** | integer `0..3` on the tenant (replaces the `free/payg/premium` string) | players, CCU, rate, storage, projects — as one bundled ladder |
| Feature **grants** | existing `feature_grants` table | relay/TURN, dedicated fleet, matchmaker, chat, managed SMTP — orthogonal booleans |
| Per-axis **override** | existing override pattern (`internal/ratelimit/overrides.go`), latent | one-off exceptions for lopsided tenants; added when a real customer forces it |

Class and features are **independent axes**: a free-class tenant can hold a
feature grant; a high-class tenant is never forced to buy a feature it doesn't
use.

## Governing decisions

1. **Unmetered by default.** A fresh self-host applies no caps. The class column
   exists but is ignored until an operator sets `enforce_quotas=true` (managed
   mode). The ladder is a mechanism OSS ships, not a policy it imposes — this
   preserves the zero-config self-host guarantee.

2. **DoS rate-limiting is separate from quota enforcement.** The token bucket
   stays on always as an abuse guard, even when quotas are unmetered.

3. **Free = tier_0 with a generous success cap.** The ceiling is high enough
   that only a clearly-monetizing game hits it; the wall is an upgrade
   conversation, not a paywall. Consequence, accepted deliberately: capacity
   upgrades are *not* the main revenue. Revenue comes from features, managed
   operations, and support. The ladder is a guardrail for the top of the
   distribution.

4. **Single ladder now; per-axis overrides latent.** One class picks a default
   bundle for all axes. The class→limits mapping is kept as a named-field table
   (players, CCU, rate, storage) — not one opaque scalar — so adding per-axis
   overrides later is additive, not a refactor. Overrides get built the day a
   lopsided tenant (e.g. a 2M-player async game objecting to a CCU cap) appears.

5. **Grace + notify at the wall; block new growth only.** Warn at ~80% and 100%.
   Existing players are never dropped. Only new growth is blocked: new signups,
   new object writes past the storage cap. CCU remains the one hard cap — a
   live concurrency count can't be graced — but it is softened by a **burst
   bucket**: tenants may hold up to 2× their sustained cap for a ~10-minute
   budget (refilling over ~1 h below sustained), so launch spikes and
   reconnect storms don't hit a wall.

6. **Numbered classes.** `0..3`, free = 0. The OSS schema carries no
   value/price judgment; sortable and extensible. Human labels ("Indie",
   "Studio") and dollars live entirely in the commercial layer.

## Class bundles

Amended 2026-07-14. Enforced only when `enforce_quotas=true`. Numbers are
guardrails, tuned once real usage telemetry exists. ⚠️ marks a limit the launch
infra cannot honor at full utilization — see `docs/capacity-and-launch.md`;
acceptable because enforcement is per-tenant opt-in and these ceilings exist to
stop runaway abuse, not to promise capacity.

| Axis | tier_0 (free) | tier_1 | tier_2 | tier_3 |
|---|--:|--:|--:|--:|
| Registered players | 100k | 500k | 2M | talk-to-us |
| CCU sustained | 2,500 | 10,000 ⚠️ | 25,000 ⚠️ | custom |
| CCU burst ceiling (2×, ~10 min budget) | 5,000 | 20,000 | 50,000 | custom |
| Rate (req/s sustained) | 250 | 1,000 | 2,500 | 10,000 |
| Rate burst (bucket capacity, 2×) | 500 | 2,000 | 5,000 | 20,000 |
| Object storage | 5 GB | 25 GB | 100 GB | 500 GB |
| Projects | 3 | 10 | 20 | ∞ |
| Relay sessions / month (managed relay only) | 1,000 | 10,000 | 100,000 | custom |

The relay-session allowance meters managed-relay credential issuances
(`relay_session_usage`, `internal/relaymeter`) and only bites for tenants
holding the `p2p_relay` grant with quotas enforced — BYO relay is never
metered. Warn at 80%/100%, then refuse only new issuance; in-flight TURN
sessions are never dropped; resets each calendar month. tier_0's value is
dormant until the free dev relay slice ships (`docs/recent-pricing-tier.md`).
tier_0's CCU cap now sits under the 5,000 aggregate warn tripwire
(`docs/capacity-and-launch.md`), so a single free tenant can no longer
consume the whole platform alert envelope.

Rate-axis sizing rule (2026-07-18): **sustained rate = CCU cap / 10**, the
reference chatty-game envelope (one DB action per player per 10 s), so every
class can actually fill its advertised CCU cap — the CCU headline is never
rate-bound. tier_3's 10,000 starting value exceeds the rule's 5,000 floor.

Design intent: **CCU is generous everywhere** — idle sockets are nearly free
(`docs/capacity-and-launch.md`); a mid-size indie hit fits inside tier_0
without ever seeing the ceiling. Upgrade pressure comes from **projects,
storage, and registered players** — team-size- and success-shaped axes.

`internal/ratelimit/connection_cap.go` and `internal/ratelimit/tier.go`
migrate from the 3-string enum to the `0..3` integer with these values
(`docs/temp/tier-rework.md` M1/M3).

## Features (orthogonal, via `feature_grants`)

Default OFF, enabled per tenant/project. At launch:

| Feature | Launch status | Reason |
|---|---|---|
| TURN relay (managed) | OFF — BYO only | No dedicated relay VM; embedded relay would contend with app+DB on the shared box |
| Dedicated fleet (managed) | OFF — BYO only | k3s/Agones torn down; no managed fleet infra |
| Matchmaker | ON (default-on scope) | No extra infra cost |
| Chat bridges | Per grant | Plugin subprocesses |
| Managed SMTP relay | Per grant | Covers non-Go devs |

Relay egress is **not metered**: OVH bundles bandwidth, so per-GB billing is
pointless. When managed relay ships (post dedicated relay VM), gate it with a
boolean grant plus a soft monthly session/GB allowance under the same
grace+notify policy — never per-GB billing.

## Commercial mapping (out of the OSS repo)

- **OSS repo:** exposes the control-panel admin actions — set tenant class
  (int), toggle a feature grant, and review tenant **change requests** (a
  tenant asks for a class upgrade or a feature; a platform admin approves —
  auto-applying the change — or denies with an optional reason; see
  `docs/temp/tier-rework.md` M5). Grants already carry
  `ApprovedByControlPanelUserID` + `Reason` for audit. Zero dollars anywhere.
- **Launch commercial layer = Stripe Payment Link + a human.** Customer pays →
  operator bumps class and flips grants by hand. At low customer counts this is
  faster than automation and puts you in direct contact with every early
  customer.
- **Automate later** only when manual bumps become a chore — decided design:
  separate `ggscale-billing` service + neutral entitlement hooks,
  `docs/billing-stripe.md` (supersedes the build-tag alternative).
- **Downgrade/cancel** respects grace: set class to 0, but an over-cap tenant
  gets the notify+grace path, not an instant wall.

## Enforcement counting (what's needed to turn quotas on)

| Axis | Countable today | Work |
|---|---|---|
| CCU | yes — `connection_cap.go` tracks live count | burst bucket (`tier-rework.md` M3) |
| Rate | yes — token bucket | ladder update only |
| Players | yes — `COUNT` on `project_players` at signup | trivial (M2) |
| Projects | yes — count at creation | trivial (M2; no check exists today) |
| Storage bytes | no — needs a running per-tenant byte sum on write | **in scope** (M4, was fast-follow) |
| 80% warn | no — needs a periodic job (fits River, next to GC) | small (M4) |

Launch posture: all axes above ship with the tier rework
(`docs/temp/tier-rework.md`). Enforcement stays opt-in per tenant
(`enforce_quotas`), but **managed production enables it for every tenant**:
the prod deploy sets the enforce-by-default server config so approved signups
land with `enforce_quotas=true`; self-host default remains false (zero-config
guarantee).

## Open / deferred

- All ladder numbers are provisional; revisit after telemetry.
- The ⚠️ CCU values exceed current host capacity at full utilization
  (`docs/capacity-and-launch.md`) — acceptable as opt-in guardrails; resize
  before promising them as managed capacity.
- Per-axis overrides: build on first lopsided customer.
- Stripe automation: build when manual bumps become a chore.
