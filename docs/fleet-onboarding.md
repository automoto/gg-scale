# Onboarding a New Game to ggscale

How to bring a new game online as a tenant of ggscale: what you write,
what ggscale gives you, and where the responsibilities sit. The
pattern is deliberately uniform — every game looks the same from
ggscale's point of view, so the platform doesn't grow per-game special
cases.

If you're trying to understand the existing doomerang integration as a
reference, read `work/doomerang-mp/docs/gameserver.md` (in the
doomerang-mp repo) alongside this doc — it shows the same pattern
applied to a concrete game.

---

## What ggscale provides

| Concern | ggscale gives you | You don't have to |
|---|---|---|
| Player auth | player sessions, OAuth/email login, session JWT verification | Build your own login |
| Matchmaking | `POST /v1/matchmaker/tickets`, ticket queue, bucket workers, claim/commit semantics | Run your own queue |
| Server allocation | Agones-backed allocator, region-aware selectors, drain hooks | Touch the Kubernetes API |
| Server registration | `/v1/fleet/*` for non-Agones servers; transport-agnostic address handoff | Run your own server-directory |
| Leaderboards / storage | Per-project tables, score submission, retrieval | Wire your own DB |
| Multi-tenancy | One ggscale instance hosts many games as separate tenants | Stand up your own ops stack per game |

What's **not** provided: the game itself. ggscale never sees gameplay
packets. The dedicated game-server you ship is where the simulation
runs.

---

## The four pieces you write

For each game you bring online:

### 1. A dedicated game-server binary

Listens on a transport (WebSocket, UDP, or both), runs the
authoritative simulation, accepts player connections, and broadcasts
state. **Implements the Agones SDK pattern** if you're running on the
Agones backend: `Ready` on startup, periodic `Health`, watch for the
`Shutdown` state and drain on transition. See
`work/doomerang-mp/docs/gameserver.md` and
`work/doomerang-mp/server/cmd/server/agones.go` for the reference shape.

### 2. A Fleet manifest

A Kubernetes Fleet CR under `infra/k8s/fleets/<your-game>.yaml`. Picks your
transport, port policy, replicas, region label, drain budget, and
image. Operators apply it with kubectl or GitOps; ggscale never
creates Fleets — it only allocates from them.

Minimum viable manifest, drawn from `infra/k8s/fleets/doomerang.yaml`:

```yaml
apiVersion: agones.dev/v1
kind: Fleet
metadata:
  name: your-game
  namespace: default
spec:
  replicas: 2          # FleetAutoscaler tuning is a follow-up
  template:
    metadata:
      labels:
        ggscale.region: us-east-1   # required for region-scoped allocation
    spec:
      ports:
        - name: default
          portPolicy: Dynamic
          containerPort: 7373
          protocol: TCP             # or UDP, or TCPUDP — see Transport below
      health:
        initialDelaySeconds: 5
        periodSeconds: 5
        failureThreshold: 3
      template:
        spec:
          terminationGracePeriodSeconds: 60
          containers:
            - name: your-game-server
              image: <registry>/your-game-server@sha256:<digest>
```

Pin the image by digest in prod so a force-pushed tag can't change
what allocates.

### 3. A client SDK integration

Use `github.com/automoto/ggscale-go` (Go), the C# SDK, or speak the
HTTP API directly. The client flow is uniform across games:

1. Authenticate the player — get a session token.
2. `POST /v1/matchmaker/tickets` with `fleet: <your-game>`, region,
   game-mode, attributes (lobby size, party id, whatever you need).
3. Poll the ticket (or subscribe to the WS hub) until `status: matched`.
4. Read `match_address` and `protocol_hint` from the response.
5. Dial that address using the protocol your game uses.
6. Submit scores via the leaderboard SDK at match end if you want them
   on the global board.

### 4. (Optional) Custom backend / plugin

If you don't want Agones, ggscale ships a `fleet.Backend` interface
(`internal/fleet/backend.go`) with a Docker backend and a gRPC plugin
shim. You can register your own server lifecycle by implementing it.
The matchmaker doesn't care which backend produced the address.

---

## Transport choice

Your game picks its transport, your Fleet manifest declares it, the
allocator surfaces it in the matchmaker response. ggscale doesn't
enforce or translate — each game speaks its own protocol on the wire.

| Transport | When to pick | Fleet `protocol:` value |
|---|---|---|
| **WebSocket** (TCP) | You need browsers in the player population. Trades worst-case latency under packet loss for universal reach (no install, no firewall traversal). | `TCP` |
| **Raw UDP** | Native-only PC/console game where you control the client install. Best feel for high-tick action games. | `UDP` |
| **Both, on one pod** | Game wants browser players AND native players to share matches. Server has to handle two listeners. | `TCPUDP` |
| **WebRTC** | You need UDP-like delivery in the browser. Heavier (TURN/signaling), worth it only if web latency really matters and you have ops bandwidth. | (out of scope today; needs a custom backend) |

### `protocol_hint` on the matchmaker response

Every ticket response includes a `protocol_hint` field carrying the
allocated Fleet's protocol, lower-cased (`"tcp"`, `"udp"`, `"tcpudp"`,
or empty):

```json
{
  "id": 123,
  "status": "matched",
  "match_address": "10.0.0.5:7654",
  "protocol_hint": "tcp",
  "matched_at": "2026-06-08T18:00:00Z"
}
```

What `protocol_hint` is **for**: cross-game launchers, dashboards, and
defense-in-depth in clients that handle multiple games. A unified
launcher can show "TCP/WebSocket" vs "UDP" badges; a client built for
a known game can sanity-check the protocol it expects matches what it
got.

What `protocol_hint` is **not**: the source of truth for what to dial.
Your game's client is built for a specific protocol and ultimately
trusts that. `protocol_hint` is empty when the backend can't determine
it (older allocations, plugins that don't surface protocol) — clients
must continue to work with an empty hint.

---

## Scale: from 4-player demos to 64–128 player matches

Transport and player count are independent levers. What changes when
you go large:

| Concern | Lever | Notes |
|---|---|---|
| Server CPU per tick × player count | Fleet pod `resources.limits.cpu` | Profile per-tick cost early; size with headroom for projectile/AI peaks. |
| Bandwidth per tick × player count | Fleet pod network budget; consider interest management server-side | At 60 Hz × 128 players × full state, bandwidth dominates. Don't send every entity to every player. |
| Lobby size | `attributes` field on the ticket request | Matchmaker buckets tickets until your target count is hit before allocating. |
| Region locality | `ggscale.region` label per cluster | Players bucketed by region; cross-region matches are a separate matchmaker policy. |
| Drain time | `terminationGracePeriodSeconds` on the pod template | Bump for longer match modes; `Server.Drain` waits up to its own `drainTimeout` (default 30 s) then stops the loop. |
| Replica count | Fleet `replicas` + `FleetAutoscaler` | Start with fixed `replicas` while bringing the game online; add the autoscaler once CCU justifies it. |

You don't need a new ggscale primitive for 128 players — the existing
matchmaker, allocator, and Agones backend scale with whatever pod
sizes you configure. What you do need is real load tests against your
specific game's tick cost.

---

## Per-game responsibilities split

| Concern | Game team | ggscale platform | Operator (cluster/Agones) |
|---|---|---|---|
| Game logic, physics, scoring | ✅ | | |
| Anti-cheat / authoritative simulation | ✅ | | |
| Server binary + Dockerfile | ✅ | | |
| Fleet manifest | ✅ writes | reads/allocates | applies via kubectl/GitOps |
| Matchmaker ticket API | uses | provides | |
| Player auth | uses | provides | |
| Leaderboard / storage | uses (SDK) | provides | |
| Region labels per cluster | configures | uses in selector | applies per-cluster |
| Pod lifecycle (start/scale/stop) | watches via Agones SDK | reads state | runs Agones |
| Image publish | runs `make docker-push` (or equivalent) | | mirrors registry if needed |

---

## A common-mistakes checklist

| Mistake | Consequence | Fix |
|---|---|---|
| `protocol: UDP` in the manifest for a TCP/WS server | Agones allocates a UDP host-port mapping for a TCP listener; clients can't reach the server | Match the manifest's `protocol:` to what your server actually listens on |
| Missing `ggscale.region` label on the pod template | Region-scoped allocation returns "no available game server" | Add the label per cluster; match it on the matchmaker request's `region` |
| `:latest` tag in the prod Fleet manifest | A force-push to `:latest` changes what gets allocated mid-flight | Pin by digest in prod (`@sha256:…`) |
| No Agones SDK `Ready` call | Pod stays in `Scheduled` forever; allocator can't find it | Call `sdk.Ready()` at startup; see doomerang's `agones.go` |
| Drain callback runs synchronously inside the watcher | Subsequent state updates queued for up to drain timeout | Dispatch drain in its own goroutine, guarded by `sync.Once` |
| `terminationGracePeriodSeconds` smaller than `drainTimeout` | Pod gets SIGKILLed mid-drain, in-flight match dropped | Make pod grace ≥ Drain's bounded wait |

---

## Further reading

- `work/doomerang-mp/docs/gameserver.md` — concrete walk-through of one
  game implementing this pattern end-to-end.
- `internal/fleet/agones/backend.go` — the allocator that talks to
  Agones; understand it before debugging "why didn't I get a server."
- `internal/matchmaker/` — ticket queue, worker, claim/commit
  semantics. Worth reading if you're tuning lobby sizes or buckets.
- Agones overview: <https://agones.dev/site/docs/overview/>
- Agones SDK guide: <https://agones.dev/site/docs/guides/client-sdks/>
