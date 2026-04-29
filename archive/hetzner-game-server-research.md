# Hetzner Cloud for Game Server Infrastructure: Research Summary

## Overview

This document summarizes our evaluation of Hetzner Cloud as the primary
infrastructure provider for the bw-ops multiplayer gaming platform MVP. Hetzner
was selected after evaluating OVH Cloud (unmetered bandwidth but APAC throttling
and 2026 price hikes) and Digital Ocean (hard 2 Gbps throughput cap per droplet,
no game-oriented features). The full multi-provider market analysis is in
`cloud-hosting-servers-research.md`.

## Why Hetzner Cloud Works for Our MVP

### Dedicated vCPU at the Best Price in the Market

Multiplayer game servers are fundamentally single-thread bound. The game loop
(physics, entity interpolation, AI, input validation) runs sequentially — a
server with many slow cores will drop ticks before a server with fewer fast
cores. Hetzner's dedicated vCPU line (CCX series) uses AMD EPYC processors
with consistent, un-shared CPU time at price points that undercut every
comparable provider.

Our target, the **CCX23**, delivers:

| Spec | Value |
|------|-------|
| vCPU | 4 dedicated (AMD EPYC) |
| RAM | 16 GB |
| Storage | 160 GB NVMe |
| Monthly cost | €31.49 |
| Included traffic (EU) | 20 TB |
| Included traffic (US) | 1 TB |

For comparison, a similarly specced dedicated-vCPU instance on Digital Ocean
(General Purpose `g-4vcpu-16gb-intel`) costs roughly the same but comes with a
hard 2 Gbps network throughput ceiling and no path to higher bandwidth. OVH's
equivalent VPS tier was recently repriced upward by ~30% in April 2026 due to
global RAM shortages.

The dedicated vCPU guarantee is critical. Shared-vCPU plans (even Hetzner's own
CPX line) risk noisy-neighbor interference — a co-tenant's CPU burst can cause
our game loop to miss its tick window, producing rubber-banding and
desynchronization for all connected players.

### Bandwidth Economics

Outbound traffic is the silent budget killer for game servers. A single
100-player server transmitting ~100 MB/hour/player generates approximately
**7.3 TB of egress per month**. On AWS at $0.08/GB, that costs $584/month in
bandwidth alone.

Hetzner's model:

| Location | Included traffic | Overage rate |
|----------|-----------------|--------------|
| EU (Falkenstein, Nuremberg, Helsinki) | 20 TB | €1.00/TB |
| US (Ashburn, Hillsboro) | 1 TB | €1.00/TB |
| Singapore | 0.5 TB | €7.40/TB |

For our MVP (US + EU):

- **EU server pushing 7.3 TB/month**: fully covered by the 20 TB allowance.
  Zero overage.
- **US server pushing 7.3 TB/month**: 1 TB included, 6.3 TB overage at
  €1/TB = **€6.30/month extra**. Negligible.

The €1/TB overage rate is the cheapest in the alternative cloud market. Vultr
charges $0.01/GB ($10/TB) — ten times more. The only provider with better raw
economics is OVH (unmetered in EU/NA), but OVH throttles APAC locations to
10 Mbps after a 1-4 TB soft cap, making it unusable for any future Asian
expansion.

Incoming traffic and internal (private network) traffic are free and unmetered.

### Location Coverage for MVP Regions

Hetzner operates in six locations that cover our three MVP regions:

| Location code | Geography | Use case |
|---------------|-----------|----------|
| `ash` | Ashburn, Virginia (US East) | Primary US East game servers |
| `hil` | Hillsboro, Oregon (US West) | US West game servers |
| `fsn1` | Falkenstein, Germany (EU) | European game servers |
| `nbg1` | Nuremberg, Germany (EU) | Backup / overflow EU |
| `hel1` | Helsinki, Finland (EU) | Nordic / Eastern EU coverage |
| `sin` | Singapore (APAC) | Not recommended (see constraints) |

Ashburn and Hillsboro provide sub-40ms coverage to the vast majority of the US
and Canadian populations. Falkenstein peers directly into DE-CIX Frankfurt, the
largest internet exchange in Europe, delivering excellent latency across the EU
and UK.

### DDoS Protection Included

All Hetzner Cloud servers receive automatic L3/L4 DDoS mitigation at no extra
cost. The scrubbing infrastructure uses Arbor and Juniper hardware to detect and
filter volumetric attacks (SYN floods, DNS/NTP amplification, UDP floods). This
runs always-on — no opt-in required, no per-instance surcharge.

For comparison, Vultr charges $10/month per instance for DDoS protection.
Digital Ocean offers no dedicated DDoS protection on basic droplets at all.

### Developer Experience

The `hcloud` CLI is clean, well-documented, and installable via Homebrew. Key
workflow:

```
brew install hcloud
hcloud context create myproject    # paste API token
hcloud server create --name srv1 --type ccx23 --image ubuntu-24.04 --location ash --ssh-key bw-laptop
hcloud server list
hcloud server delete srv1
```

Additional tooling:
- **Ansible collection** (`hetzner.hcloud`) for dynamic inventory and cloud
  modules
- **Official Go and Python libraries** for custom automation
- **Project-scoped API tokens** for isolation between environments
- Default SSH user is `root` with key-based auth — no password dance on first
  login

### Other Advantages

- **NVMe storage standard** on all plans — 6x faster I/O than SATA SSD,
  important for game worlds that stream chunk data from disk
- **99.9% uptime SLA**
- **Snapshots and backups** available for quick server cloning
- **Cloud Firewalls** (stateful, cloud-level) available in addition to UFW
- **Private networks** between servers in the same location at no cost
- **Hourly billing** — spin up test servers without committing to a full month

---

## Constraints and Limitations

### No Game-Specific DDoS Protection

Hetzner's DDoS mitigation is generalized L3/L4 scrubbing. It handles volumetric
floods (SYN, UDP amplification) but **does not perform deep packet inspection
on game traffic**. It cannot distinguish between a legitimate player's UDP game
packet and a spoofed attack packet at the application layer.

OVH Cloud is the only provider offering purpose-built Game DDoS protection —
a Layer 7 firewall that understands game engine protocol state and can filter
malicious UDP from the first packet, backed by 17 Tbps of scrubbing capacity.

**Risk level**: Low for MVP (small player base, low profile target). Becomes
a real concern at scale or if the game attracts competitive communities where
DDoS attacks are common (rival server operators, disgruntled players).

**Mitigation path**: When scaling, evaluate adding OVH bare metal nodes with
Game DDoS as a protective frontend, or use a third-party service like
Cloudflare Spectrum or Path.net for UDP proxying.

### US Traffic Allowance is Low

US locations include only 1 TB of traffic (vs 20 TB in EU). For game servers
generating 7+ TB/month, overage is guaranteed. The overage rate (€1/TB) is
extremely cheap, so the financial impact is minimal (~€6-7/month extra per
server), but it means the "included traffic" number on US plans is essentially
cosmetic for our workload.

**Why it matters**: Budget forecasting should assume zero included traffic in US
and plan for full overage billing. At €1/TB, even 50 TB/month costs only €50 —
still far cheaper than any hyperscaler.

### Singapore Is Expensive and Limited

Singapore (`sin`) has only 0.5 TB included traffic with a **€7.40/TB** overage
rate — 7.4x more expensive than US/EU. A game server pushing 7.3 TB/month in
Singapore would cost approximately €50/month in overage alone on top of the
server cost.

**Impact**: Hetzner is not viable as our APAC game server provider. Per the
multi-cloud strategy in `cloud-hosting-servers-research.md`, Vultr is the
recommended provider for Singapore, Tokyo, and Seoul due to its flat $0.01/GB
($10/TB) worldwide overage and dense APAC data center presence.

### Limited Geographic Footprint

Hetzner operates in only 6 locations worldwide. There is **no presence** in:
- East Asia (Tokyo, Seoul) — the world's most latency-sensitive gaming markets
- Latin America (São Paulo) — fastest-growing gaming region globally
- Middle East (Dubai, Riyadh) — expanding cloud gaming market
- Oceania (Sydney) — needed for Australian players

This means Hetzner cannot be a single-provider global solution. The multi-cloud
expansion strategy (Vultr for APAC, OCI for LatAm/ME) documented in
`cloud-hosting-servers-research.md` is not optional — it is architecturally
required for any audience beyond North America and Western Europe.

### April 2026 Price Increase

Hetzner raised prices approximately 30-35% across all cloud server tiers in
April 2026, citing global RAM shortages driven by AI/GPU demand. While Hetzner
remains the cheapest dedicated-vCPU option in the market even after the
increase, the gap has narrowed. Future price adjustments are possible if the
component shortage persists.

### No Bare Metal Cloud Offering

Hetzner Cloud is virtualized only. They offer dedicated (bare metal) servers
through a separate product line (Hetzner Robot/Dedicated), but those are not
managed through the `hcloud` CLI or API and have longer provisioning times
(hours, not seconds).

For the absolute highest game server performance (5+ GHz boost clocks, 3D
V-Cache), OVH's bare metal Game servers with Ryzen 9000X 3D processors remain
unmatched. This is only relevant for extremely demanding game engines at scale.

### Nested Virtualization Not Supported

Hetzner Cloud servers cannot run virtual machines inside them. This is
irrelevant for game server workloads but worth noting if container-in-VM
architectures were ever considered.

---

## Trade-offs Summary

| Decision | What we gain | What we give up |
|----------|-------------|-----------------|
| Hetzner over OVH | Better price/perf post-2026 hikes, simpler CLI, faster provisioning | Unmetered bandwidth, game-specific L7 DDoS protection, bare metal Ryzen 9000X 3D |
| Hetzner over Digital Ocean | Dedicated vCPU, no throughput cap, cheaper bandwidth overage | Slightly larger global footprint (DO has more locations, though none matter for APAC gaming) |
| Hetzner over Vultr | Cheaper bandwidth (€1/TB vs $10/TB), included DDoS protection | 32 global locations vs 6, flat worldwide overage pricing, high-frequency VX1 instances |
| Dedicated vCPU (CCX) over shared (CPX) | Guaranteed tick-rate consistency, no noisy-neighbor risk | ~2x the cost per server |
| Multi-cloud strategy (later) | Global low-latency coverage, provider-appropriate DDoS | Operational complexity, multiple billing relationships, heterogeneous tooling |

---

## Conclusion

Hetzner Cloud is the right choice for our US + EU MVP infrastructure. It
delivers the best dedicated-CPU-to-cost ratio in the market, NVMe storage,
and bandwidth overage rates so low that even the reduced US traffic allowance
is financially immaterial. The `hcloud` CLI and Ansible collection provide a
clean provisioning workflow that integrates with our existing repo structure.

The primary risks — lack of game-specific DDoS protection and limited global
footprint — are acceptable for an MVP phase targeting North American and
European players. Both are addressed in our documented multi-cloud expansion
strategy: OVH for DDoS-hardened frontends at scale, Vultr for APAC edge nodes,
and OCI for cost-effective LatAm/Middle East presence.
