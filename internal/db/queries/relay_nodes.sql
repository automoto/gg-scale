-- name: CreateRelayNode :one
INSERT INTO relay_nodes (region, turn_urls, lifecycle_url)
VALUES (sqlc.arg(region), sqlc.arg(turn_urls)::text[], sqlc.arg(lifecycle_url))
RETURNING *;

-- name: ListRelayNodes :many
-- eligible uses the same rule and clock as ListEligibleRelayNodes, so the
-- admin list agrees with what credentials serve.
SELECT sqlc.embed(relay_nodes),
       (state = 'known'
        AND last_probe_ok
        AND last_lifecycle_state = 'active'
        AND last_probe_at > now() - make_interval(secs => sqlc.arg(max_age_seconds)::double precision))::boolean AS eligible
FROM relay_nodes
ORDER BY id;

-- name: ListKnownRelayNodes :many
SELECT *
FROM relay_nodes
WHERE state = 'known'
ORDER BY id;

-- name: RetireRelayNode :one
-- Retiring is idempotent and keeps the row. A retired node is never probed
-- or advertised again.
UPDATE relay_nodes
SET state = 'retired',
    updated_at = CASE WHEN state = 'retired' THEN updated_at ELSE now() END
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: RecordRelayNodeProbe :exec
-- Stores what the probe read; a probe that read nothing clears the lifecycle
-- columns. The state filter keeps a probe that raced a retire from touching
-- the retired row.
UPDATE relay_nodes
SET last_probe_at = now(),
    last_probe_ok = sqlc.arg(ok),
    last_probe_error = sqlc.narg(probe_error),
    last_lifecycle_state = sqlc.narg(lifecycle_state),
    last_active_allocations = sqlc.narg(active_allocations),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND state = 'known';

-- name: ListEligibleRelayNodes :many
-- Eligible: known, last probe succeeded, reported active, and probed within
-- the freshness window.
SELECT *
FROM relay_nodes
WHERE state = 'known'
  AND last_probe_ok
  AND last_lifecycle_state = 'active'
  AND last_probe_at > now() - make_interval(secs => sqlc.arg(max_age_seconds)::double precision)
ORDER BY id;
