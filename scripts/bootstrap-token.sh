#!/usr/bin/env bash
# Make ./data/bootstrap.token readable by the current host user without sudo.
# The server writes it as uid 65532 mode 0600 on the bind-mounted data dir.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

TOKEN_FILE="./data/bootstrap.token"
SERVER_UID=65532
SERVER_GID=65532
WAIT_SECONDS="${BOOTSTRAP_TOKEN_WAIT:-15}"

OPTIONAL=0
PREPARE=0
for arg in "$@"; do
  case "$arg" in
    --if-present) OPTIONAL=1 ;;
    --prepare) PREPARE=1 ;;
    *)
      echo "usage: $0 [--if-present|--prepare]" >&2
      exit 2
      ;;
  esac
done

run_priv() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
    return
  fi
  if command -v sudo >/dev/null 2>&1; then
    sudo "$@"
    return
  fi
  "$@"
}

claim_token() {
  run_priv chown "$(id -u):$(id -g)" "$TOKEN_FILE"
  run_priv chmod 0600 "$TOKEN_FILE"
}

prepare_token() {
  if [ ! -f "$TOKEN_FILE" ]; then
    return 0
  fi
  run_priv chown "$SERVER_UID:$SERVER_GID" "$TOKEN_FILE"
  run_priv chmod 0600 "$TOKEN_FILE"
}

wait_for_token() {
  local waited=0
  while [ ! -f "$TOKEN_FILE" ]; do
    if [ "$waited" -ge "$WAIT_SECONDS" ]; then
      return 1
    fi
    sleep 1
    waited=$((waited + 1))
  done
  return 0
}

missing_message() {
  echo "bootstrap.token not found — run 'make up' first, then retry 'make bootstrap-token'." >&2
}

if [ "$PREPARE" -eq 1 ]; then
  prepare_token
  exit 0
fi

if [ ! -f "$TOKEN_FILE" ]; then
  if ! wait_for_token; then
    missing_message
    if [ "$OPTIONAL" -eq 1 ]; then
      exit 0
    fi
    exit 1
  fi
fi

claim_token
echo "bootstrap token is readable at ${TOKEN_FILE#./} (mode 0600)"
