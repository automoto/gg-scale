# Contributing to ggscale

*Note: We are not accepting external pull requests at this time due to PR spam but please feel free to file an issue or request a feature in our discussion board.*

Thank you for considering contributing. ggscale is Apache 2.0 licensed; by
contributing you agree your contribution is licensed under the same terms.

## Local development

All make targets use plain `docker` / `docker compose`; any Docker engine works
(Docker Desktop, a Linux daemon, Colima).

1. Install Go 1.26.5+, Docker, `golangci-lint`, `govulncheck`.
2. `cp .env.example .env`
3. `make test-integration` runs the fast Testcontainers suite.
4. `make up` brings the basic stack up.
5. `curl localhost:8080/v1/healthz` should return `200`.
6. `make test-e2e` runs the exhaustive and live-stack suite.

**Fleet feature (beta, not part of GA):** the k3s + Agones dev stack lives in
a separate repo. This repo's dev tooling covers the GA features; the fleet
feature ships only via the Agones and plugin backends.

See `docs/ARCHITECTURE.md` for what's actually running.

### Troubleshooting

- `docker-credential-desktop: executable file not found in $PATH` — your
  `~/.docker/config.json` sets `"credsStore": "desktop"` but the helper isn't on
  PATH. With Docker Desktop installed, restore the symlink:
  `ln -s /Applications/Docker.app/Contents/Resources/bin/docker-credential-desktop /usr/local/bin/`.
  Without Docker Desktop (e.g. Colima only), change `credsStore` to
  `osxkeychain` (macOS) or delete the line.

## Workflow

- Branch from `main`.
- Tests first: write a failing test before implementation. Use `testify/assert`,
  AAA pattern, table-driven tests where they fit. Test names describe behavior:
  `should_return_empty_when_no_items`.
- Code must be `go fmt` clean and pass `make lint` (`golangci-lint`);
  `make check` runs lint + unit tests together, the same gate CI applies.
- See `docs/testing.md` for the suite boundary and reliability guidelines.
- Open a PR; CI runs lint and unit tests first, then runs integration and
  end-to-end tests concurrently on separate Linux runners.

## Reporting issues

Use GitHub Issues. Include reproduction steps, the version (`git rev-parse HEAD`),
and relevant logs (`docker compose logs ggscale-server`).
