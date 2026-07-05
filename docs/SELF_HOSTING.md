# Self-hosting ggscale

ggscale runs on any Linux box with Docker and Postgres. Game server hosting has two paths — start with the simpler one and move up when you outgrow a single host.

> **v1.0 note — fleet hosting is opt-in and off by default.** Everything on this
> page is gated behind `FEATURE_FLEET_ENABLED` (default `false`); the embedded
> P2P relay is gated behind `FEATURE_P2P_RELAY_ENABLED` (also `false`). The basic
> stack (`make up`) serves auth, player accounts, storage, leaderboards, and the
> social layer without any of this. To run game servers, set
> `FEATURE_FLEET_ENABLED=true` **and** a `FLEET_BACKEND` (`docker` / `agones` /
> `plugin:<name>`) — the two are validated together, so setting a backend while
> the feature is off is rejected at startup.

## Which path?

```
Do you need to span more than one VM?
  No  → Docker backend (compose/fleet-docker.yml). Single host, dynamic containers.
  Yes → Agones backend (compose/fleet-agones.yml). k3s + Agones, multi-node, autoscaling.
```

Both paths use the same client SDK call (`AllocateServer()`); switching backends is an operator change, not a code change.

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

## Docker backend (single host)

ggscale spawns and tears down game-server containers on demand via the Docker socket. Single host, no Kubernetes.

Good for: community runs, LAN events, studios that want to skip Kubernetes, anything that fits on one VM.

### Setup

1. Copy `.env.example` to `.env` and set:

   ```
   GAME_SERVER_PUBLIC_IP=203.0.113.1   # your VPS public IP
   FLEET_SECRET=<strong-random-secret>
   DOCKER_GAMESERVER_IMAGE=ghcr.io/ggscale/doomerang-server:latest
   ```

2. Start the stack:

   ```sh
   make up-fleet-docker
   ```

   Starts postgres, mailhog, and ggscale-server with `FLEET_BACKEND=docker` and the Docker socket mounted.

3. Check everything came up:

   ```sh
   docker compose -f compose/fleet-docker.yml ps
   ```

4. Open UDP port 7654 (or whatever your game-server image listens on) for clients. Read the UDP security section below before doing this.

### Capacity

The Docker backend handles burst capacity within the limits of one host — RAM, CPU, file descriptors, and how many concurrent containers your daemon can manage. When you need to span multiple hosts, move to the Agones backend.

---

## Agones backend (multi-host)

k3s + Agones. Multi-node, Kubernetes-native lifecycle (Ready → Allocated → Draining), autoscaling.

Move here when you need to span multiple machines or want fleet autoscaling.

To migrate from the Docker backend: bring up k3s + Agones (`make up-fleet-agones && make agones-install`), set `FLEET_BACKEND=agones` in `.env`, restart `ggscale-server`. Game client code stays the same.

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
| Docker → Agones | `make up-fleet-agones && make agones-install`, `FLEET_BACKEND=agones`, restart | No client changes |
| Agones → Docker | `FLEET_BACKEND=docker`, restart ggscale-server | No client changes |

The game client SDK always calls `AllocateServer()`. Which backend handles it is an operator concern — game code doesn't change between backends.
