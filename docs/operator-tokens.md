# Operator tokens

These are the bearer tokens an operator can configure. Each one guards a
different surface, so a leak exposes only that surface. They are not the
Game Project API keys or player sessions that the `/v1` API uses. Generate
each token with `openssl rand -hex 32`. Tokens other than
`RELAY_METRICS_TOKEN` also accept a `<NAME>_FILE` variant for file-mounted
secrets.

| Token | Set on | Guards | Who holds it | If it leaks |
|---|---|---|---|---|
| `LIFECYCLE_TOKEN` | API servers and relay nodes | `GET /lifecycle`, `POST /lifecycle/drain`, `POST /lifecycle/resume` ([ops-endpoints.md](ops-endpoints.md)) | Deploy tooling | Anyone who can reach the lifecycle routes can drain nodes or put them back into service. |
| `RELAY_PROBE_TOKEN` | API servers, optional | Nothing on the server itself. The relay node probe sends it to each node's `GET /lifecycle`. Unset, the probe sends the API server's own `LIFECYCLE_TOKEN`. | Nobody: the servers use it | The same as the relay nodes' `LIFECYCLE_TOKEN`. |
| `PLATFORM_ADMIN_TOKEN` | API servers | `/v1/admin/*`: register, list and retire relay nodes ([relay-ops.md](relay-ops.md)) | Operators who deploy relay nodes | The holder can change which relays every client is sent to, and make the API servers probe chosen private addresses. |
| `METRICS_AUTH_TOKEN` | API servers | `/metrics`. Required in production unless `METRICS_AUTH_DISABLED=true`. | Prometheus | Operational metrics are exposed. |
| `RELAY_METRICS_TOKEN` | Relay nodes | The relay's `/metrics` | Prometheus | Relay metrics are exposed. |
| `ENTITLEMENT_API_TOKEN` | API servers, with `ENTITLEMENT_API_ENABLED=true` | `/internal/entitlements` | The billing service | The holder can change any tenant's tier and features. |

The control panel's bootstrap token is generated for the first sign-up on a
fresh install and is never configured by hand. `RELAY_SHARED_SECRET` is a
signing key shared by API servers and relay nodes for TURN credentials. It is
not a bearer token.

## One lifecycle token or two

By default the whole fleet shares one `LIFECYCLE_TOKEN`: the API servers use it
for their own lifecycle routes and send it when they probe relay nodes. That is
the least to configure.

A relay node faces the internet on its TURN ports, so it is more exposed than
an API server. To keep a compromised relay host from holding a token that can
drain the API servers, give the relay nodes their own `LIFECYCLE_TOKEN` and set
`RELAY_PROBE_TOKEN` on the API servers to that value. If the two do not match,
every probe gets `401`, the nodes stop being listed, and each API server logs
the mismatch once per node.

## What a typical install sets

- Single host, relay embedded in the API server: `METRICS_AUTH_TOKEN` in
  production. Add `LIFECYCLE_TOKEN` to drain the server for deploys.
- Dedicated relay nodes: `LIFECYCLE_TOKEN` everywhere, `PLATFORM_ADMIN_TOKEN`
  on the API servers, plus the metrics tokens your scraper uses.
