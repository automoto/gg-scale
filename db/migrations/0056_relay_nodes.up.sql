-- Registry of standalone relay nodes the API servers advertise in relay
-- credentials. A platform admin registers and retires nodes through the admin
-- API; the RelayNodeProbe job writes the last_probe_* columns from each node's
-- GET /lifecycle. Platform-global, so there is no tenant column and no RLS.
CREATE TABLE relay_nodes (
    id                      BIGSERIAL PRIMARY KEY,
    region                  TEXT NOT NULL CHECK (region <> ''),
    turn_urls               TEXT[] NOT NULL CHECK (cardinality(turn_urls) > 0),
    lifecycle_url           TEXT NOT NULL CHECK (lifecycle_url <> ''),
    state                   TEXT NOT NULL DEFAULT 'known' CHECK (state IN ('known', 'retired')),
    last_probe_at           TIMESTAMPTZ,
    last_probe_ok           BOOLEAN NOT NULL DEFAULT false,
    -- Why the last probe failed, as a stable reason such as "timeout" or
    -- "status 503". NULL when it succeeded or has not run.
    last_probe_error        TEXT,
    -- The state and active_work the last probe read. NULL when the last probe
    -- read no valid /lifecycle document.
    last_lifecycle_state    TEXT,
    last_active_allocations BIGINT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One live registration per lifecycle URL; a retired node's URL can be
-- registered again.
CREATE UNIQUE INDEX relay_nodes_known_lifecycle_url_idx
    ON relay_nodes (lifecycle_url)
    WHERE state = 'known';

GRANT SELECT, INSERT, UPDATE ON relay_nodes TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE relay_nodes_id_seq TO ggscale_app;
