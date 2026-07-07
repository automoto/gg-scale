#!/usr/bin/env bash
# Generates openapi.yaml for the /v1 JSON API using ehabterra/apispec.
#
# Upstream apispec (v0.3.5 and main @ cb336e36) crashes with a stack overflow
# on this codebase (unguarded recursion over a cyclic tracker tree) and
# mislabels implicit-200 responses. Until those fixes land upstream, this
# script builds a locally patched binary from a pinned commit plus
# scripts/patches/apispec-fixes.patch, then runs it with apispec.yaml.
#
# Usage: scripts/gen-openapi.sh   (from the repo root; writes ./openapi.yaml)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APISPEC_COMMIT=cb336e367628b7bee411bd0aca5bcacee2c8c473
BUILD_DIR="${REPO_ROOT}/bin/.apispec-src"
BIN="${REPO_ROOT}/bin/apispec-patched"

if [ ! -x "$BIN" ]; then
    echo "building patched apispec (${APISPEC_COMMIT:0:12})..."
    rm -rf "$BUILD_DIR"
    mkdir -p "$BUILD_DIR"
    git -C "$BUILD_DIR" init --quiet
    git -C "$BUILD_DIR" fetch --quiet --depth 1 https://github.com/ehabterra/apispec "$APISPEC_COMMIT"
    git -C "$BUILD_DIR" checkout --quiet FETCH_HEAD
    git -C "$BUILD_DIR" apply "${REPO_ROOT}/scripts/patches/apispec-fixes.patch"
    (cd "$BUILD_DIR" && go build -o "$BIN" ./cmd/apispec)
    rm -rf "$BUILD_DIR"
fi

cd "$REPO_ROOT"
"$BIN" \
    --dir . \
    --config apispec.yaml \
    --output openapi.yaml \
    --exclude-package "github.com/ggscale/ggscale/internal/dashboard" \
    --exclude-package "github.com/ggscale/ggscale/internal/players" \
    --exclude-package "github.com/ggscale/ggscale/internal/webassets" \
    --contact-name "" --contact-email "" --contact-url ""

# Merge hand-maintained operations apispec cannot extract (see
# docs/openapi-generation.md "Known gaps").
go run ./scripts/openapi-overlay openapi.yaml openapi-overlay.yaml

echo "wrote openapi.yaml"
