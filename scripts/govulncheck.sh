#!/usr/bin/env bash
#
# govulncheck wrapper that suppresses a small allowlist of OSV IDs and fails
# CI on anything else. Lives outside Go so any developer can edit the
# allowlist without touching the CI workflow.
#
# Allowlist entries MUST carry a rationale comment and a re-evaluation cue
# (e.g. "drop when upstream ships X"). Suppressions are not "set and
# forget" — each one is a known liability we accept until upstream lands a
# fix.
#
# Usage: scripts/govulncheck.sh [paths...]   (default: ./...)
#
# Requires: govulncheck on $PATH (install with `go install
#   golang.org/x/vuln/cmd/govulncheck@latest`) and jq.

set -euo pipefail

# Accepted (known) vulnerabilities. KEEP THIS LIST SHORT.
# Each entry: "GO-ID  one-line-rationale  // re-evaluate when ..."
ACCEPTED=(
    # github.com/docker/docker — Moby AuthZ plugin bypass on oversized request
    # bodies. We use the Docker SDK as a CLIENT against a trusted local
    # daemon (compose stack) or operator-managed daemon. The bug is a
    # daemon-side AuthZ plugin issue we cannot trigger from the client side.
    # Re-evaluate when Moby ships a tagged fix and the SDK requires it.
    "GO-2026-4887"

    # github.com/docker/docker — Off-by-one in plugin privilege validation.
    # Again a daemon-side issue. Same re-evaluation trigger as 4887.
    "GO-2026-4883"

    # github.com/pion/dtls/v2 — Random nonce generation for AES-GCM. Pulled
    # in transitively via github.com/pion/turn/v3 for the TURN relay. The
    # vuln applies to DTLS server-side crypto; our relay use is exposed,
    # but no upstream patch exists in v2.x. Re-evaluate when pion/dtls
    # ships v2.x patch or when we move to pion/dtls v3.
    "GO-2026-4479"
)

PATHS=("$@")
if [[ ${#PATHS[@]} -eq 0 ]]; then
    PATHS=("./...")
fi

if ! command -v govulncheck >/dev/null 2>&1; then
    echo "govulncheck: not on \$PATH; install with: go install golang.org/x/vuln/cmd/govulncheck@latest" >&2
    exit 127
fi
if ! command -v jq >/dev/null 2>&1; then
    echo "jq: not on \$PATH; install jq before running this wrapper" >&2
    exit 127
fi

# Run govulncheck with JSON output. It exits non-zero when anything is
# affected, so capture and inspect rather than relying on the exit code.
raw=$(govulncheck -format=json -mode=source "${PATHS[@]}" || true)

# Each JSON record is a line. Extract OSV ids of "finding"s that report a
# trace (govulncheck's signal that the called-code analysis confirmed the
# vulnerability is reachable, not just imported).
affecting=$(
    printf '%s\n' "$raw" \
        | jq -rs '
            [
                .[]
                | .finding?
                | select(. != null)
                | select(.trace != null and (.trace | length) > 0)
                | .osv
            ]
            | unique
            | .[]
        '
)

# Filter out accepted entries.
accepted_pattern=$(printf '|%s' "${ACCEPTED[@]}")
accepted_pattern="${accepted_pattern#|}"

unaccepted=$(printf '%s\n' "$affecting" | grep -vxE "$accepted_pattern" || true)

if [[ -n "$unaccepted" ]]; then
    echo "govulncheck: unaccepted vulnerabilities found:" >&2
    printf '  %s\n' $unaccepted >&2
    echo >&2
    echo "Either patch the affected code/deps, or add the OSV id to ACCEPTED" >&2
    echo "in scripts/govulncheck.sh with a written rationale." >&2
    echo >&2
    echo "Full govulncheck output:" >&2
    printf '%s\n' "$raw" \
        | jq -rs '
            .[]
            | select(.finding? != null)
            | select(.finding.trace != null and (.finding.trace | length) > 0)
            | "\(.finding.osv)\t\(.finding.trace[0].module // "stdlib")"
        ' >&2 || true
    exit 1
fi

# Report what we suppressed so it stays visible in the build log.
if [[ -n "$affecting" ]]; then
    echo "govulncheck: passed (accepted suppressions):"
    printf '  %s\n' $affecting
else
    echo "govulncheck: passed (no findings)."
fi
