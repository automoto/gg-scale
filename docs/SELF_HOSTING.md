# Self-hosting ggscale

ggscale runs on any Linux box with Docker and Postgres. Game server hosting has three tiers — start with the simplest one and move up when you need to.

## Which tier?

```
Do you need more than ~20 concurrent game server instances?
  No  → Tier 0 (compose). No Kubernetes, static containers.
  Yes → Do you need multiple VMs?
          No  → Tier 1 (Docker SDK). Single host, dynamic containers. (Phase 2)
          Yes → Tier 2 (Agones). Kubernetes, multi-node. (Phase 2)
```

Tiers 1 and 2 ship in Phase 2. The migration path between tiers requires no game client code changes.

---

## How allocation works

ggscale is not in the real-time game traffic path. It handles allocation and gets out of the way:

```
Game client          ggscale               Game server
     │                   │                      │
     │  POST /v1/fleet/  │                      │
     │  allocate         │                      │
     │──────────────────►│                      │
     │                   │  (picks a server,    │
     │                   │   records session)   │
     │  {host, port,     │                      │
     │   session_token}  │                      │
     │◄──────────────────│                      │
     │                                          │
     │  UDP/TCP direct connection               │
     │─────────────────────────────────────────►│
     │                                          │
     │  (game traffic — ggscale not involved)   │
```

Set `GAME_SERVER_PUBLIC_IP` to the public IP or hostname of the machine running the game server container. That's what ggscale hands to clients.

---

## Tier 0 — Compose (static containers)

Good for: up to ~20 concurrent servers, community runs, LAN events, studios that want to skip Kubernetes for now.

The constraint is fixed capacity. Each game server instance is a service you define in the compose file — no autoscaling. When you need more servers, edit the file and restart.

### Setup

1. Copy `.env.example` to `.env` and set:

   ```
   GAME_SERVER_PUBLIC_IP=203.0.113.1   # your VPS public IP
   FLEET_SECRET=<strong-random-secret>
   ```

2. Start the stack:

   ```sh
   make up-gameserver
   ```

   Starts postgres, ggscale-server, and one `doomerang-server` instance.

3. Check everything came up:

   ```sh
   docker compose -f docker-compose.yml -f ops/docker-compose.gameserver.yml ps
   ```

4. Open UDP port 7654 on your host. Read the UDP security section below before doing this.

### Running multiple instances

Uncomment the `doomerang-server-2` block in `ops/docker-compose.gameserver.prod.yml`, increment the host port, and run the three-file compose:

```sh
docker compose \
  -f docker-compose.yml \
  -f ops/docker-compose.gameserver.yml \
  -f ops/docker-compose.gameserver.prod.yml \
  up -d --wait
```

The prod overlay also adds `restart: unless-stopped` and CPU/memory limits.

This works well up to ~20 instances. Beyond that, managing static port assignments by hand gets tedious — that's when Tier 1 makes more sense.

---

## Tier 1 — Docker SDK backend (Phase 2)

ggscale spawns and tears down game server containers on demand via the Docker socket. Single host, no Kubernetes.

Move here when you need burst capacity or want ggscale to manage container lifecycle automatically.

To migrate from Tier 0: set `FLEET_BACKEND=docker` and restart `ggscale-server`. The Docker socket is already mounted in `ops/docker-compose.gameserver.yml`, so no compose changes are needed. Game client code stays the same.

---

## Tier 2 — Agones (Phase 2)

k3s + Agones. Multi-node, Kubernetes-native lifecycle (Ready → Allocated → Draining), autoscaling.

Move here when you need to span multiple machines or want fleet autoscaling.

To migrate from Tier 1: run `make up-k8s && make agones-install`, set `FLEET_BACKEND=agones`, restart `ggscale-server`. Game client code stays the same.

---

## UDP security

Read this before opening a UDP port to the internet.

### Docker bypasses host firewalls

Docker writes iptables rules directly. `ufw deny 7654/udp` does not block a Docker-mapped port — Docker's rules win regardless of what ufw says. Once Docker maps the port, it's publicly reachable.

You have a few options:

- Leave `GAME_SERVER_BIND_ADDR=0.0.0.0` (default) and rely on session-token auth and iptables rate limiting.
- Set `GAME_SERVER_BIND_ADDR=<specific-ip>` to bind to one NIC only.
- Set `DOCKER_OPTS="--iptables=false"` in the Docker daemon config and own all iptables rules yourself.

### Game servers must validate session tokens

UDP has no handshake. Anyone who finds port 7654 can send packets. Your game server needs to check the ggscale-issued session token on the first packet and drop anything that fails before sending any response.

Skipping this creates two problems: unauthorized connections, and the server becomes a reflector in DDoS attacks (attacker spoofs a victim's IP; server sends a large response to the victim).

### Rate limiting

UDP is cheap to flood. Add an iptables rate limit on each game server port:

```sh
iptables -A INPUT -p udp --dport 7654 -m limit --limit 500/s --limit-burst 1000 -j ACCEPT
iptables -A INPUT -p udp --dport 7654 -j DROP
```

Tune the limits for your traffic levels. Persist with `iptables-save` / `netfilter-persistent`.

### DDoS protection at the edge

For titles with a large player base, put a scrubbing service in front at the network level. OVH's AntiDDoS is included on all VPS plans. Hetzner has similar built-in protection. Cloudflare Magic Transit is an option for higher-traffic targets.

### `FLEET_SECRET` is not player auth

`FLEET_SECRET` authenticates the game server container to ggscale's internal fleet API (Phase 2). Players authenticate via the session token from the allocation response — `FLEET_SECRET` never touches that flow. Treat it like a service account password and don't share it with players.

---

## Migration path

| From | To | What to change |
|------|----|----------------|
| Tier 0 → Tier 1 | `FLEET_BACKEND=docker`, restart ggscale-server | No client changes |
| Tier 1 → Tier 2 | `make up-k8s && make agones-install`, `FLEET_BACKEND=agones`, restart | No client changes |
| Any → Tier 0 | `FLEET_BACKEND=static` (or unset), restart | No client changes |

The game client SDK always calls `AllocateServer()`. Which backend handles it is an operator concern — game code doesn't change between tiers.
