#!/usr/bin/env bash
# Cloud Agent install phase: prepare durable, source-derived state for the
# gg-scale dev stack. Idempotent — safe to re-run against a warm snapshot.
#
# The pinned system toolchain lives in Dockerfile. This script runs after the
# repository is checked out, so it only prepares repository-derived state.
# Per-boot work (starting Docker and preparing runtime paths) lives in start.sh.
set -euo pipefail

log() { printf '\n=== %s ===\n' "$1"; }

main() {
  local repo_root
  repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  cd "$repo_root"

  if [ ! -f .env ]; then
    log "Creating .env from .env.example"
    cp .env.example .env
  fi

  log "Downloading Go modules"
  go mod download

  log "Warming the Go build cache"
  go build ./...

  log "install.sh complete"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
