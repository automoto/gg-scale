#!/usr/bin/env bash
# Cloud Agent install phase: prepare durable, source-derived state for the
# gg-scale dev stack. Idempotent — safe to re-run against a warm snapshot.
#
# What lives here (vs. start.sh): everything that is stable and can be baked
# into the environment build snapshot — system packages, pinned tools, Go
# module cache, and a compiled package cache. Per-boot work (starting the
# Docker daemon, fixing nested-container networking) lives in start.sh.
set -euo pipefail

GOLANGCI_LINT_VERSION="v2.11.4" # keep in sync with .github/workflows/ci.yml
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

log() { printf '\n=== %s ===\n' "$1"; }

# ── Docker Engine + Compose (needed by make up / integration / e2e) ────────
if ! command -v docker >/dev/null 2>&1; then
  log "Installing Docker Engine + Compose plugin"
  sudo install -m 0755 -d /etc/apt/keyrings
  sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  sudo chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
    | sudo tee /etc/apt/sources.list.d/docker.list >/dev/null
  sudo apt-get update -qq
  # fuse-overlayfs backs the Docker storage driver inside the nested Cloud
  # Agent VM (the kernel refuses nested native overlay mounts). The
  # --force-conf* dpkg options keep the fuse3 package from stopping on an
  # interactive /etc/fuse.conf conffile prompt in the non-interactive build.
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    -o Dpkg::Options::=--force-confdef \
    -o Dpkg::Options::=--force-confold \
    docker-ce docker-ce-cli containerd.io docker-buildx-plugin \
    docker-compose-plugin fuse-overlayfs
else
  log "Docker already installed ($(docker --version))"
fi

# ── Docker daemon config for the nested VM ─────────────────────────────────
# Native overlay mounts fail in the nested VM, so use the fuse-overlayfs graph
# driver with the classic (non-containerd) snapshotter.
log "Writing /etc/docker/daemon.json"
sudo mkdir -p /etc/docker
sudo tee /etc/docker/daemon.json >/dev/null <<'JSON'
{
  "storage-driver": "fuse-overlayfs",
  "features": { "containerd-snapshotter": false }
}
JSON

# ── Let the dev user talk to the daemon without sudo (make up uses `docker`) ─
sudo groupadd -f docker
sudo usermod -aG docker "$(id -un)"

# ── golangci-lint (pinned to the version CI enforces) ──────────────────────
if ! command -v golangci-lint >/dev/null 2>&1 || \
   ! golangci-lint version 2>/dev/null | grep -q "${GOLANGCI_LINT_VERSION#v}"; then
  log "Installing golangci-lint ${GOLANGCI_LINT_VERSION}"
  curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sudo sh -s -- -b /usr/local/bin "${GOLANGCI_LINT_VERSION}"
else
  log "golangci-lint already installed ($(golangci-lint version 2>/dev/null | head -1))"
fi

# ── Repo bootstrap ─────────────────────────────────────────────────────────
if [ ! -f .env ]; then
  log "Creating .env from .env.example"
  cp .env.example .env
fi

log "Downloading Go modules"
go mod download

log "Warming the Go build cache"
go build ./...

log "install.sh complete"
