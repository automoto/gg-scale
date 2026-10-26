# Operator tokens

These are the bearer tokens an operator can configure. They are separate from
the Game Project API keys and player sessions that the `/v1` API uses.
Generate each one with `openssl rand -hex 32`. Every token below except
`RELAY_METRICS_TOKEN` also accepts a `<NAME>_FILE` variant for file-mounted
secrets.

| Token | Set on | Guards | Who holds it | If it leaks |
|---|---|---|---|---|
| `LIFECYCLE_TOKEN` | API servers and relay nodes | That process's `GET /lifecycle`, `POST /lifecycle/drain` and `POST /lifecycle/resume` ([ops-endpoints.md](ops-endpoints.md)) | Deploy tooling | Anyone who can reach those routes can drain the process or put it back into service. |
| `RELAY_PROBE_TOKEN` | API servers | Nothing on the API server. The relay node probe sends it to every registered node's `GET /lifecycle`, so it must equal the relay nodes' `LIFECYCLE_TOKEN`. Unset, the probe sends no header, which only works for relay nodes without a `LIFECYCLE_TOKEN`. | The API servers, plus deploy tooling, which holds the same value as the relay nodes' `LIFECYCLE_TOKEN` | The same as the relay nodes' `LIFECYCLE_TOKEN`: relay nodes can be drained or resumed. |
| `PLATFORM_ADMIN_TOKEN` | API servers | `/v1/admin/*`: register, list and retire relay nodes ([relay-ops.md](relay-ops.md)) | Operators who deploy relay nodes | The holder can change which relays every client is sent to, and can make the API servers probe chosen loopback or private addresses. By registering a URL they control, they also receive `RELAY_PROBE_TOKEN`. |
| `METRICS_AUTH_TOKEN` | API servers | `/metrics`. Required in production unless `METRICS_AUTH_DISABLED=true`. | Prometheus | Operational metrics are exposed. |
| `RELAY_METRICS_TOKEN` | Relay nodes | The relay's `/metrics`. Optional; unset leaves it open. | Prometheus | Relay metrics are exposed. |
| `ENTITLEMENT_API_TOKEN` | API servers, optional | `/internal/entitlements`, mounted only with `ENTITLEMENT_API_ENABLED=true`. When unset, the server generates the token and stores it in the database. | The billing service | The holder can change any tenant's tier and features. A generated token is also in database backups. |

The control panel's bootstrap token is generated for the first sign-up on a
fresh install and is never configured by hand. `RELAY_SHARED_SECRET` is a
signing key shared by API servers and relay nodes for TURN credentials, not a
bearer token.

## Keep the relay nodes' token separate

The probe sends `RELAY_PROBE_TOKEN` to every `lifecycle_url` a platform admin
registers, and often over plain `http` on a private network. Treat that value
as one that relay hosts and platform admins hold. Give the relay nodes their
own `LIFECYCLE_TOKEN`, distinct from the API servers' one, and set
`RELAY_PROBE_TOKEN` on the API servers to the relay nodes' value. Then the
worst a leaked relay token does is drain relays, which an admin can already
take out of service by retiring them. An API server logs a warning at startup
when `RELAY_PROBE_TOKEN` equals its own `LIFECYCLE_TOKEN`. Keep
`RELAY_HEALTH_ADDR` on a private network, or serve it behind TLS, so the token
does not cross an untrusted network.

If `RELAY_PROBE_TOKEN` does not match a relay node's `LIFECYCLE_TOKEN`, the
node's probes get `401`, it stops being listed, and the mismatch is logged
once per node across the cluster. Rotating the relay nodes' token therefore
means updating `RELAY_PROBE_TOKEN` on the API servers at the same time. A node
accepts only one token at a time, so expect each node to be unlisted until both
sides match.

## What a typical install sets

- Single host, relay embedded in the API server: `METRICS_AUTH_TOKEN` in
  production. Add `LIFECYCLE_TOKEN` to drain the server for deploys.
- Dedicated relay nodes: `PLATFORM_ADMIN_TOKEN` and `LIFECYCLE_TOKEN` on the
  API servers; a different `LIFECYCLE_TOKEN` on the relay nodes, repeated as
  `RELAY_PROBE_TOKEN` on the API servers; plus the metrics tokens your scraper
  uses.
