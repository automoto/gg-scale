# Pricing tier research & recommendation

Research + recommendation, 2026-07-18. Companion to
`docs/pricing-strategy.md` (ladder decision record) and
`docs/capacity-and-launch.md` (infra grounding). **Adopted and implemented
2026-07-18** (same day): the §3 ladder is live in `quota.LimitsForClass`,
`ratelimit.LimitsForTier`, and `ratelimit.ConnectionCapForClass` (tier_3
kept at its prior 10k/20k rate start by explicit decision), and the relay
session metering from §4 shipped as `internal/relaymeter` + migration 0016
(warn 80%/100% emails, refusal past allowance, monthly reset), wired into
`/v1/relay/credentials`. Dollar amounts remain outside the repo (Stripe /
marketing only). Ops follow-up: review the bw-ops aggregate CCU tripwires
against the new per-tenant caps.

Sources: deep research run 2026-07-18 (20 sources fetched live, 25 top
claims adversarially verified, 23 confirmed / 2 refuted) plus infra costs
from `bw-ops` (`database-dokku.md`, project docs).

## 1. Verified competitor pricing (fetched live 2026-07-18)

| Vendor | Bills on | Free tier | Key price points |
|---|---|---|---|
| Heroic Cloud (Nakama) | dedicated CPU cores | none — free = self-host OSS | prices calculator/sales-gated; ~$600/mo entry (2021 third-party anecdote); support sold separately: Studio Basic $2,000, Standard $6,000 (billing period not stated on page) |
| Photon Realtime | peak CCU (traffic bundled) | 20 CCU, dev-only | $95/mo @ 500 CCU, $185/mo @ 1,000 CCU (~$0.19/CCU); Premium $0.29/CCU, $580 min, to 50k CCU; Fusion/Quantum $0.50/CCU |
| brainCloud | API calls + flat per-app plans | 100 DAU / 1,000 accounts (dev sandbox) | Lite $15, Standard $30, Business $99/mo (Plus $25/$50/$199), unlimited DAU; ~$10–12/M call overage |
| Unity Gaming Services | per-service usage meters | Relay: 50 avg CCU; Lobby: 10 GiB; Cloud Save: 5 GiB + 1M reads/writes | Relay $0.16/CCU + $0.09–0.16/GiB; auth free; leaderboards free "for a limited time" |
| Edgegap | vCPU-minutes + egress | relays: 50 CCU / 160 GB (labeled "free trial") | servers $0.00115/vCPU-min + $0.10/GB; relays $0.14/peak CCU; hosted matchmaker ~$22/$105/$395/mo |

Market-structure events (2026): Hathora shut down May 5, 2026 (acquired by
Fireworks AI; customers migrated to Nitrado's GameFabric). Unity exited
Multiplay hosting March 31, 2026 (licensed to Rocket Science, which
publishes no rate card). Edgegap is the remaining transparently priced
game-server host — one fewer transparent competitor, and displaced
customers are actively shopping.

Key structural findings:

- **No standard billing metric exists.** Photon bills peak CCU, Unity bills
  usage meters, Edgegap bills compute+egress, brainCloud bills API calls,
  Heroic bills CPU capacity with explicitly no DAU/MAU/CCU limits. **Nobody
  bills on registered players** — that axis has no market precedent as a
  billing meter (fine as a fair-use guardrail; risky as the thing customers
  pay for).
- **Our free tier is 2–3 orders of magnitude more generous** than every
  verified competitor free tier (5k CCU free ≈ $1,450/mo of capacity at
  Photon list price; 250x Photon's free CCU, ~250x brainCloud's account
  cap). Coherent only under the sell-ops-not-capacity strategy — but it
  means the CCU wall will almost never trigger an upgrade; conversion must
  come from other axes and feature grants.
- **Table-stakes features are free at incumbents** (Unity: auth free,
  leaderboards free "for a limited time", lobby/cloud-save nearly free).
  Auth/leaderboards/lobbies fill out the bundle but can't carry a price
  tag; relay, fleets, ops, and support can.
- **Managed relay has a converged market rate**: $0.14–0.16 per CCU-month
  plus ~$0.09–0.16/GB egress (Unity, Edgegap).
- Refuted framings (excluded): brainCloud as a billing-metric outlier; a
  niche-wide shift to negotiated contracts.

Coverage gaps: no claims about PlayFab, Epic Online Services, AccelByte,
Beamable, LootLocker, or Supabase/Appwrite/Convex survived verification —
the "compete with free EOS" question is answered only indirectly via Unity
data, and remains the biggest open competitive question.

## 2. Hardware-cost check (bw-ops)

Current production: **~$354/mo** — 2 app hosts + monitoring (b3-8,
~$43/mo each), 2 database hosts (b3-16 ~$88 + 250 GB gen2 volume ~$24 ≈
$112/mo each). Levers: app b3-8→b3-16 +$45/mo; DB b3-16→b3-32 +$88/mo;
volume grows online.

Per `docs/capacity-and-launch.md`: **CCU is nearly free by architecture**
(idle socket = tens of KB in the Go hub; heartbeats refresh the cache,
never Postgres). Cost is driven by DB-touching actions/sec — which the
**rate axis caps**, and by storage — which the **storage axis caps**. So
the expensive resources are bounded regardless of CCU generosity.

Marginal infra cost if a tenant *fully* utilizes each current tier
(illustrative, using the capacity doc's unmeasured 1-action/10s envelope):

| Tier fully utilized | Forces | Marginal cost | Research-recommended price |
|---|---|--:|--:|
| tier_0: 5k CCU, 150 req/s, 5 GB | maybe one app-host resize | ~$20–50/mo | $0 |
| tier_1: 20k CCU, 1k req/s, 25 GB | ~a b3-16 of app capacity + DB share | ~$100–150/mo | $149–249 |
| tier_2: 50k CCU, 5k req/s, 100 GB | ~3–4 app b3-16s + DB pair → b3-32 | ~$500–700/mo | $999–1,999 |

Verdict: **the ladder is not economically dangerous** — margins hold even
at full utilization at the researched prices, and typical utilization is
far below cap. Three specific misalignments, though:

1. **tier_0's per-tenant CCU cap (5,000) equals the platform-wide warning
   tripwire (5,000 aggregate CCU)** — one free tenant can consume the
   entire alert envelope and force a paid resize. The free cap should sit
   below the platform warn line.
2. **250k registered players free disarms the stated upgrade lever.** The
   axis costs ~nothing (bytes are bounded by the storage cap), but at 250k
   a clearly-successful game never converts; competitors convert 100–1000x
   earlier.
3. **tier_2 at full utilization needs ~2x current total infra.** Fine only
   if priced ≥$999 and provisioned on demand when a real tier_2 tenant
   appears — never ahead of time.

## 3. Recommended ladder (proposal)

Reshaped toward a cheap self-serve rung (Supabase-style $25–49 gap; early
customers are small and $999 tiers sell rarely on day one). Keeps classes
0..3 — a `LimitsForClass` + docs change only, no schema churn:

| Axis | tier_0 (free) | tier_1 ~$49/mo | tier_2 ~$249/mo | tier_3 (custom, from ~$999) |
|---|--:|--:|--:|--:|
| Registered players | 100k | 500k | 2M | custom |
| CCU sustained (burst 2×) | 2,500 (5k) | 10k (20k) | 25k (50k) | custom |
| Rate req/s (burst) | 250 (500) | 1k (2k) | 2.5k (5k) | 10k (20k) |
| Object storage | 5 GB | 25 GB | 100 GB | 500 GB+ |
| Projects | 3 | 10 | 20 | ∞ |
| Managed relay | BYO only | BYO only | included (fair-use) | included, custom |
| Support | community | email | priority | SLA contract ($1.5k–5k/mo, sold separately) |

Rationale per rung:

- **tier_0**: still 25–100x more generous than any competitor free tier
  (2,500 CCU vs Photon's 20), now under the aggregate tripwire; worst-case
  cost ~$50/mo is acceptable CAC. Comparison caveat: Photon's free CCU are
  *relayed* CCU (all gameplay traffic through their servers, hence their
  60 GB bundle), while our free CCU are backend/realtime-services sockets
  — relay is BYO at tier_0. That asymmetry is exactly why our free tier is
  affordable to give away, but it means a free dev doesn't get
  "multiplayer out of the box"; for the backend-only portion the honest
  comparables are brainCloud (1,000 accounts, no relay) and Unity (auth
  free, relay metered separately), against which 100x+ still holds.
- **tier_1 ~$49** (raised from the initial $39 recommendation,
  2026-07-18): still inside the $25–49 band where indie self-serve
  conversion happens, and 25% more revenue per customer at low
  in-band elasticity. At 1,000 req/s sustained full utilization the worst
  case is roughly an app-host share (~$80–150/mo) — underwater only for a
  tenant camping at 100% continuously, which the tripwires surface with
  days of runway; that tenant is a clearly-successful game and gets the
  upgrade conversation / per-axis override, per the existing
  wall-as-conversation policy. Deliberately NOT insured against via a
  higher list price or a lower rate cap.
- **Rate-axis sizing rule (2026-07-18)**: sustained rate = CCU cap / 10
  (the reference chatty-game envelope, 1 DB action/player/10 s), so every
  class can actually fill its advertised CCU cap — the CCU headline is
  never rate-bound. Applied to all classes; tier_3's 10k start already
  exceeds the rule's 5k floor.
- **tier_2 ~$249**: ~20x under Photon's equivalent-capacity list price
  while covering its ~$150–250 worst-case cost; the "real studio" rung.
- **tier_3**: where sunsetting publishers land; they are really buying the
  support/SLA contract (price deliberately under Heroic's $2k/$6k).

Alternative shape (if fewer, bigger deals are preferred and current caps
kept unchanged): tier_1 $149–249, tier_2 $999–1,999 — expect a long empty
funnel between free and $149 early on.

Pricing principles carried over from the research:

- **Never bill on registered players** (no market precedent; per-player
  pricing resentment mirrors the Unity runtime-fee backlash). Keep it a
  soft fair-use ceiling with the grace+notify path.
- **Don't fight Unity/EOS on table stakes** — monetize relay, fleets, ops,
  support.
- Round published prices to market conventions ($49 / $249, or $199 /
  $1,499 in the alternative shape).

## 4. Relays: include as the paid thing, not the free thing

Yes — managed relay is the **best-margin product available**: market rate
$0.14–0.16/CCU + $0.09–0.16/GB while OVH bundles our bandwidth, so relay
traffic's marginal cost is ~zero. Infra cost is one dedicated relay VM per
region (b3-8 ~$43/mo each; the capacity doc already rules out sharing the
app box).

- **BYO relay stays free at every tier** — already built; the OSS
  credibility story.
- **Managed relay is a feature grant** bundled into tier_2+, sellable as a
  ~$29–49/mo add-on to tier_1, with a soft fair-use session/GB allowance
  under the existing grace+notify policy — consistent with the "never
  per-GB billing" rule in `docs/pricing-strategy.md`.
- **Stand up relay VMs only on the first paying request** (~$86/mo for two
  regions), matching the current launch posture ("ship relay grants OFF,
  BYO only").
- **Optional, after relay VMs exist**: add a ~25–50 CCU dev-grade relay
  allowance to tier_0 — matches the Unity Relay / Edgegap free-slice norm
  (both 50 CCU), costs ~nothing marginal on already-running VMs (OVH
  bandwidth bundled), and funnels devs into the paid relay grant. Never
  stand up infra for this alone.

### How relay maps onto the tiers

Access stays on the existing orthogonal mechanism — env switch +
`p2p_relay` feature grant + `ScopeP2PRelay` key scope — and tier bundling
happens only in the commercial layer (the Stripe price carries
`feature=p2p_relay` metadata; the entitlement API applies it). No code
ties relay to class, preserving the independent-axes decision in
`docs/pricing-strategy.md`.

| Tier | Managed relay | Allowance (soft, grace+notify) |
|---|---|---|
| tier_0 | BYO always; dev slice only after relay VMs exist | ~50 concurrent / ~1,000 sessions/mo, single region, no SLA |
| tier_1 | add-on (~$29–49/mo), not included | ~10k sessions/mo |
| tier_2 | included | ~100k sessions/mo fair-use |
| tier_3 | included | custom, multi-region, SLA |

Allowance is metered in **relay sessions** (credential issuances at
`/v1/relay/credentials` — trivially countable per tenant/month), never GB
(bandwidth is bundled; per-GB is ruled out). Enforcement follows "block
new growth only": warn 80%/100%, then refuse *new* credential issuance —
in-flight TURN sessions are never dropped. Rollout order: (1) selling via
Stripe metadata works today, zero code; (2) session-allowance metering +
warn emails (small, fits the storage-quota machinery); (3) relay-only
deployment mode for the dedicated VM (the relay currently runs embedded
in ggscale-server) — build at first paying request; (4) then the free dev
slice as a low default allowance.

## 5. Caveats

- The cost table rests on the capacity doc's **unmeasured** per-player
  action rate (1 DB action / player / 10 s). Conclusions are robust to
  2–3x error because the req/s caps bind first, but measure it
  (`docs/temp/connection-capacity.md` task 6) before printing tier_3
  promises.
- Heroic's ~$600 entry is a dated third-party anecdote; their real compute
  prices are calculator-gated. Their $2k/$6k support figures carry no
  stated billing period on the page.
- Photon CCU is *peak*; Unity's is *average monthly* — cross-vendor CCU
  comparisons are directional, not unit-identical. Edgegap's 50-CCU relay
  allowance is labeled "free trial" and may be time-limited.
- High volatility: two major market exits within four months of the
  research date; Rocket Science may publish Multiplay pricing at any time;
  Unity Leaderboards is free only "for a limited time".
- EOS/PlayFab pricing and indie perception of paying vs free EOS remain
  unresearched — the largest open competitive question.

## Primary sources

- https://heroiclabs.com/pricing/
- https://www.photonengine.com/realtime/pricing
- https://getbraincloud.com/pricing/
- https://unity.com/products/gaming-services/pricing
- https://support.unity.com/hc/en-us/articles/4410136449812-How-is-the-Relay-Service-Priced
- https://edgegap.com/resources/pricing
- https://gamefabric.com/hathora (Hathora transition)
- https://www.rocketscience.gg/multiplay/ (Multiplay successor)
- `bw-ops/database-dokku.md`, `docs/capacity-and-launch.md` (infra costs)
