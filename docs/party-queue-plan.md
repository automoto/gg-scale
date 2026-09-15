# Party queue implementation (#59)

- [x] Schema, tenant isolation, party service, invites and presence.
- [x] Atomic queue entries and whole-entry grouping.
- [x] Atomic commit, rematch, disconnect and recovery.
- [x] Player APIs, OpenAPI and client recovery.
- [x] Integration and race coverage, release docs and wiki pages.

Recovered the interrupted implementation and completed it on the
`hermes/party-queue-impl-003-resume-and-finish-the-party-queue-implementation`
branch. Party enqueue stays disabled until the operator completes the
[single-cutover procedure](parties.md#operator-cutover).

## Validation

- Go 1.26.5; Linux only.
- `make check`: lint and all unit tests with the race detector.
- PostgreSQL 17 matchmaker integration tests with the race detector: all three
  modes, whole-entry claims, partial-commit rejection, concurrent cancel/kick/
  disconnect, expired presence before commit, tenant isolation, friend invites,
  code limits, rematch, allocation recovery, and solo migration.
- PostgreSQL HTTP API integration tests with the race detector: player routes,
  polling recovery, and queue/rematch without stranger membership.
- Generated OpenAPI and CI test-bucket checks.

Tests were added before the final fixes for expired presence at commit and
allocation cleanup on backends without the optional resolution-cleanup method.
The full Linux CI lanes also cover Docker-based integration and end-to-end tests.

## PR review follow-up

- [x] Code joins require only the invite code. Party locking, capacity checks,
  code-use limits, and membership insertion remain transactional. Friend invite
  acceptance still uses the version returned by the invite list.
- [x] Regression coverage checks the join schema and joins without a version
  after another party mutation. The capacity race still admits one last member.
