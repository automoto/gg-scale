# Contributing to ggscale

Thank you for considering contributing. ggscale is Apache 2.0 licensed; by
contributing you agree your contribution is licensed under the same terms.

## Local development

All make targets use plain `docker` / `docker compose`; any Docker engine works
(Docker Desktop, a Linux daemon, Colima).

1. Install Go 1.26.5+, Docker, `golangci-lint`, `govulncheck`.
2. `cp .env.example .env`
3. `make up` brings the basic stack up.
4. `curl localhost:8080/v1/healthz` should return `200`.
5. `make e2e` runs the end-to-end suite.

**Optional — Agones fleet testing:** `make up-fleet-agones && make agones-install`
adds a k3s + Agones cluster. This is the one flow that needs Colima on macOS
(`colima start --network-address`) because Docker Desktop's host networking
breaks Agones UDP; `make preflight-k8s` checks this for you.

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
- Open a PR; CI runs `lint-test`, `docker-build`, and the full `e2e` suite.

## Reporting issues

Use GitHub Issues. Include reproduction steps, the version (`git rev-parse HEAD`),
and relevant logs (`docker compose logs ggscale-server`).
