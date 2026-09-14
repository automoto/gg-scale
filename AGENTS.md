# Repository Instructions

## Code Quality

- Use early returns to reduce nesting.
- Write idiomatic Go and keep all Go code `gofmt` clean.
- Keep code simple and concise. Avoid clever abstractions unless they remove real complexity.
- Handle errors explicitly. Avoid panics unless failure is truly unrecoverable at startup.
- Add comments only where extra context is useful.
- Prefer standard-library helpers such as `unicode.IsControl`, `unicode.IsSpace`, and `net/mail.ParseAddress` over bare ASCII numeric literals.

## Testing Conventions

- Write tests before implementation for new features.
- Use Arrange-Act-Assert structure.
- Prefer one assertion per test when practical.
- Name tests for behavior.
- Use `github.com/stretchr/testify/assert` for test assertions.
- Use table-driven tests where they keep coverage concise.
- Run `make lint` after significant new code when feasible; it uses `golangci-lint`.

## Project Notes

- Go version preference: use Go 1.26.3 or newer in the 1.26 line. The current module declares Go 1.26.4.
- CI cost constraint: use Linux only in CI. macOS and Windows runners cost money.
- After completing milestone or phase work, update the relevant planning document to reflect completed tasks.

## Cloud Agent environment

- `.cursor/install.sh` and `.cursor/start.sh` provision the dev stack. `install.sh` installs Docker (fuse-overlayfs storage driver for the nested VM), the pinned `golangci-lint`, and warms Go caches; `start.sh` boots `dockerd` (there is no systemd), opens the legacy iptables `FORWARD` policy so container-to-container traffic works, and makes `./data` writable for the server bind mount.
- The Docker daemon is already running on boot — just use `make up`, `make test-integration`, `make e2e`, etc.
- The server container runs as the distroless `nonroot` uid, so the control-panel bootstrap token is written `0600`; read it with `sudo cat ./data/bootstrap.token` (it is also in `docker compose logs ggscale-server`).
