# Repository Instructions

## Code Quality

- Use early returns and keep nesting shallow.
- Write idiomatic Go.
- Keep code simple and concise. Avoid clever abstractions unless they remove real complexity.
- Handle errors explicitly. Avoid panics unless failure is truly unrecoverable at startup.
- Add comments only where extra context is useful.
- Prefer standard-library helpers such as `unicode.IsControl`, `unicode.IsSpace`, and `net/mail.ParseAddress` over bare ASCII numeric literals or hex constants like `0x20`.
- Run `make lint` (`golangci-lint`) after significant new code. All code must pass it.

## Testing Conventions

- Write failing tests before implementation for new features, then write the minimal code to pass.
- Use Arrange-Act-Assert structure.
- Prefer one assertion per test when practical.
- Name tests for behavior, for example `should_return_empty_when_no_items`.
- Use `github.com/stretchr/testify/assert` for test assertions.
- Use table-driven tests where they keep coverage concise.
- Test lanes (see `docs/testing.md`): `make test` for unit tests, `make test-integration` for the fast Testcontainers suite, `make test-e2e` for the full suite against `make up`.

## Repository Rules

- Match the Go version declared in `go.mod`. Do not bump it without asking.
- Never edit an applied migration. Add a forward migration instead.
- Do not put milestone, phase, or task numbers in code, identifiers, comments, or file names. They belong in docs only.
- Features must work with minimal config for self-hosters.
- Maintain derived and counter state in the Go write paths, not in database triggers.
- `make openapi` regenerates `openapi.yaml`. Do not edit it by hand.
- Frontend (control panel and player UI): self-hosted assets only. No CDN, no Tailwind or Alpine, no inline `<script>` or `<style>`.
- After completing a task, update the planning document that tracks it so completed work is marked done.

## Cloud Agent Environment

- `.cursor/Dockerfile` provides pinned Go, Docker Engine/CLI, and `golangci-lint` versions plus Compose. It configures the `fuse-overlayfs` storage driver and Docker access for the Cloud Agent user.
- `.cursor/install.sh` creates `.env` and warms Go caches during environment builds. `.cursor/start.sh` boots `dockerd` (there is no systemd), opens the legacy iptables `FORWARD` policy so container-to-container traffic works, and prepares `./data` for the server bind mount.
- The start hook leaves the Docker daemon ready. Use `make up`, `make test-integration`, `make e2e`, etc.
- The server container runs as the distroless `nonroot` uid, so the control-panel bootstrap token is written `0600` as uid 65532 and stays owned by that uid so the server can overwrite it on restart. Read it with `make bootstrap-token`. Server logs report the token file path but never the token value.
