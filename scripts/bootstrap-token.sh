#!/usr/bin/env bash
# Print ./data/bootstrap.token without changing ownership.
# The server writes it as uid 65532 mode 0600 so it can overwrite the file on restart.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TOKEN_FILE="./data/bootstrap.token"
WAIT_SECONDS="${BOOTSTRAP_TOKEN_WAIT:-15}"

wait_for_token() {
  local waited=0
  while [ ! -f "$TOKEN_FILE" ]; do
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

if [ ! -f "$TOKEN_FILE" ]; then
  if ! wait_for_token; then
    echo "bootstrap.token not found — run 'make up' first, then retry 'make bootstrap-token'." >&2
    exit 1
  fi
fi

if [ -r "$TOKEN_FILE" ]; then
  cat "$TOKEN_FILE"
  exit 0
fi

sudo cat "$TOKEN_FILE"
