#!/usr/bin/env bash
# Cloud Agent start phase: per-boot runtime setup. Runs every time the VM
# boots (it is NOT part of the build snapshot). Must be idempotent, reconcile
# already-running state, and return once Docker is ready.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SERVER_UID=65532
SERVER_GID=65532

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
# Cloud Agent VMs can inherit a legacy FORWARD policy of DROP. Reconcile it on
# every boot so Docker bridge traffic, such as the server reaching Postgres,
# is not rejected by the outer ruleset.
if command -v iptables-legacy >/dev/null 2>&1; then
  log "Opening iptables-legacy FORWARD policy for Docker bridges"
  sudo iptables-legacy -P FORWARD ACCEPT
fi

# The Dockerfile adds the Cloud Agent user to the docker group before the VM
# starts, so the socket can retain Docker's normal root:docker permissions.
if ! docker info >/dev/null 2>&1; then
  echo "Docker is running, but the current user cannot access its socket" >&2
  exit 1
fi

# ── Ensure the bind-mounted data dir is writable by the server container ────
# docker-compose mounts ./data into the distroless (uid 65532) server, which
# writes the control-panel bootstrap token there. Give only that uid ownership
# instead of making the host path world-writable.
log "Preparing ./data for the server bind mount"
sudo install -d -m 0755 -o "$SERVER_UID" -g "$SERVER_GID" data

log "start.sh complete — run 'make up' to launch the dev stack"
