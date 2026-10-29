# Contributing to ggscale

*Note: We are not accepting external pull requests at this time due to PR spam but please feel free to file an issue or request a feature in our discussion board.*

Thank you for considering contributing. ggscale is Apache 2.0 licensed; by
contributing you agree your contribution is licensed under the same terms.

## Local development

All make targets use plain `docker` / `docker compose`; any Docker engine works
(Docker Desktop, a Linux daemon, Colima).

1. Install Go 1.26.10+, Docker and `golangci-lint`.
2. `cp .env.example .env`
3. `make test-integration` runs the fast Testcontainers suite.
4. `make up` brings the basic stack up.
5. `curl localhost:8080/v1/healthz` should return `200`.
6. `make test-e2e` runs the exhaustive and live-stack suite.

**Fleet feature (beta, not part of GA):** the k3s + Agones dev stack lives in
a separate repo. This repo's dev tooling covers the GA features; the fleet
feature ships only via the Agones and plugin backends.

The [wiki](https://github.com/automoto/gg-scale/wiki) covers the architecture.

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
- Run `make templ-generate` and commit the regenerated `*_templ.go` files
  after changing a `.templ` file, and whenever the `github.com/a-h/templ`
  version in `go.mod` changes. Dependabot bumps it without regenerating, so
  regenerate on its branch before merging. CI fails when they are out of date.
- Open a PR; CI runs lint and unit tests first, then runs integration and
  end-to-end tests concurrently on separate Linux runners.

## Database migrations

Migrations run when the server starts. During a deploy the previous release
and the new release run against the same schema, and the previous release is
also the rollback. So every migration must keep the previous release working.

- Expand, then contract. Add new tables, columns, indexes and functions in one
  release. Remove the old ones in a later release, after no supported release
  uses them.
- A new column is nullable or has a default, so the previous release can still
  insert rows without it.
- Do not rename or drop a table, column or SQL function that the previous
  release reads, writes or calls. Add the new one, write to both (or keep both
  functions), and drop the old one in a later release. A function with a new
  argument list or return shape is a new function: keep the old signature until
  no supported release calls it (see `0054` and `0055`, which replaced one in
  place).
- New enum values and values that a wider `CHECK` constraint allows are added in
  one release and written only in a later release, after every supported
  release can read them. Otherwise a rollback reads rows it cannot parse.
- Build indexes on tables that can have traffic with
  `CREATE INDEX CONCURRENTLY IF NOT EXISTS` (and drop them with
  `DROP INDEX CONCURRENTLY`). A plain `CREATE INDEX` blocks writes to the table
  until the build ends. A migration file with several statements runs as one
  transaction and `CONCURRENTLY` cannot run in one, so the statement is the
  only one in its migration (see `0021`).
- Do not add a constraint or unique index that rows written by the previous
  release can break. First make every supported release write rows that meet
  the new rule, and clean up existing rows (`0018` removed duplicates before
  `0021` built its unique index). Otherwise the build fails, or the previous
  release gets errors for writes that used to work.
- To change a column's type or move data to a new column:
  1. Release N adds the new column and writes both columns.
  2. Backfill existing rows in small batches, each in its own short
     transaction. Do not backfill a large table in the startup migration: one
     transaction locks every row it touches until it commits, and it misses
     rows that the previous release writes while it runs.
  3. Check that no row is missing the new value before reads switch to it in
     release N+1. Drop the old column in a later release.
- If a change cannot follow these rules, it takes two releases. Say so in the
  PR description.

## Reporting issues

Use GitHub Issues. Include reproduction steps, the version (`git rev-parse HEAD`),
and relevant logs (`docker compose logs ggscale-server`).
