#!/usr/bin/env bash
# Cloud Agent start phase: per-boot runtime setup. Runs every time the VM
# boots (it is NOT part of the build snapshot). Must be idempotent, reconcile
# already-running state, and return once Docker is ready.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

log() { printf '\n=== %s ===\n' "$1"; }

# ── Start the Docker daemon (no systemd in the Cloud Agent VM) ──────────────
if sudo docker info >/dev/null 2>&1; then
  log "Docker daemon already running"
else
  log "Starting dockerd"
  sudo sh -c 'nohup dockerd >/var/log/dockerd.log 2>&1 &'
  for _ in $(seq 1 30); do
    if sudo docker info >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if ! sudo docker info >/dev/null 2>&1; then
    echo "dockerd failed to become ready; see /var/log/dockerd.log" >&2
    sudo tail -n 40 /var/log/dockerd.log >&2 || true
    exit 1
  fi
fi

# ── Fix nested-container networking ────────────────────────────────────────
# The base image ships a stale iptables-legacy ruleset whose FORWARD policy is
# DROP and which lacks Docker's per-bridge accept rules. Docker 29 programs the
# nftables backend instead, so container-to-container traffic (e.g. the server
# reaching Postgres) is silently dropped by the legacy hook. Opening the legacy
# FORWARD policy lets nftables govern forwarding as Docker intends.
if command -v iptables-legacy >/dev/null 2>&1; then
  log "Opening iptables-legacy FORWARD policy for Docker bridges"
  sudo iptables-legacy -P FORWARD ACCEPT || true
fi

# Make the socket usable this boot even before the docker group membership
# propagates to freshly spawned shells.
sudo chmod 666 /var/run/docker.sock 2>/dev/null || true

# ── Ensure the bind-mounted data dir is writable by the server container ────
# docker-compose mounts ./data into the distroless (uid 65532) server, which
# writes the control-panel bootstrap token there. A fresh, Docker-created bind
# target is root-owned, so pre-create it world-writable before `make up`.
log "Preparing ./data for the server bind mount"
sudo mkdir -p data
sudo chmod 0777 data

log "start.sh complete — run 'make up' to launch the dev stack"
