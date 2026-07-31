#!/usr/bin/env bash
# Preflight: verify the developer environment can host the dev stack
# (docker reachable, .env exists). The k8s/Agones fleet stack and its
# preflight moved to the bw-ops repo (dev/fleet-agones/).
set -euo pipefail

err=0

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

exit $err
