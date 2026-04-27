# Contributing to ggscale

Thank you for considering contributing. ggscale is Apache 2.0 licensed; by
contributing you agree your contribution is licensed under the same terms.

## Local development

1. Install Go 1.26.2, Docker (or Colima on macOS), `golangci-lint`, `govulncheck`.
2. `cp .env.example .env`
3. `make up` brings the lite stack up.
4. `curl localhost:8080/v1/healthz` should return `200`.
5. `make up-k8s` adds k3s + Agones (macOS: run `colima start --network-address` first).
6. `make e2e` runs the end-to-end suite.

See `docs/ARCHITECTURE.md` for what's actually running.

## Workflow

- Branch from `main`.
- Tests first: write a failing test before implementation. Use `testify/assert`,
  AAA pattern, table-driven tests where they fit. Test names describe behavior:
  `should_return_empty_when_no_items`.
- Code must be `go fmt` clean and pass `make lint` (`golangci-lint`).
- Open a PR; CI runs `lint-test`, `docker-build`, and the full `e2e` suite.

## Reporting issues

Use GitHub Issues. Include reproduction steps, the version (`git rev-parse HEAD`),
and relevant logs (`docker compose logs ggscale-server`).
