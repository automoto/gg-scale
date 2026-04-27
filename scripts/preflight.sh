#!/usr/bin/env bash
# Phase 0 preflight: verify the developer environment can host the dev stack.
# Usage:
#   scripts/preflight.sh        # baseline checks (docker reachable, .env exists)
#   scripts/preflight.sh k8s    # baseline + k8s-profile checks (macOS: Colima)
set -euo pipefail

mode="${1:-}"
err=0

# ─── baseline ───────────────────────────────────────────────────────────

if ! command -v docker >/dev/null 2>&1; then
  echo "preflight: docker not found in PATH" >&2
  err=1
fi

if ! docker info >/dev/null 2>&1; then
  echo "preflight: docker daemon not reachable (is Docker Desktop / Colima running?)" >&2
  err=1
fi

if [ ! -f .env ]; then
  echo "preflight: .env missing — run 'cp .env.example .env'" >&2
  err=1
fi

# ─── k8s profile (macOS Colima) ─────────────────────────────────────────

if [ "$mode" = "k8s" ] && [ "$(uname -s)" = "Darwin" ] && [ -z "${GGSCALE_SKIP_COLIMA_CHECK:-}" ]; then
  if ! command -v colima >/dev/null 2>&1; then
    cat >&2 <<EOF
preflight: the k8s profile requires Colima on macOS (Docker Desktop's host
networking is unreliable on darwin and breaks Agones UDP hostPorts).

  brew install colima
  colima start --network-address --cpus 4 --memory 8

Set GGSCALE_SKIP_COLIMA_CHECK=1 to bypass (e.g. if running k3s in a separate
Linux VM you manage directly).
EOF
    err=1
  elif ! colima status 2>&1 | grep -q "Running"; then
    cat >&2 <<EOF
preflight: Colima is installed but not running. Start it with:

  colima start --network-address --cpus 4 --memory 8

Set GGSCALE_SKIP_COLIMA_CHECK=1 to bypass.
EOF
    err=1
  fi
fi

exit $err
