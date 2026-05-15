-- Fleet allocation events power the dashboard's per-allocation timeline.
-- Each row is one StatusUpdate the backend pushed to the manager. The table
-- is a bounded ring per allocation_id (see trim trigger below) so a noisy
-- backend cannot blow the table up. Kept tenant-scoped via RLS so two
-- tenants browsing their own allocations cannot see each other's events.

CREATE TABLE fleet_allocation_events (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    allocation_id BIGINT NOT NULL REFERENCES game_server_allocations(id) ON DELETE CASCADE,
    status        allocation_status NOT NULL,
    address       TEXT NOT NULL DEFAULT '',
    err_message   TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX fleet_allocation_events_alloc_idx
    ON fleet_allocation_events (allocation_id, id DESC);

ALTER TABLE fleet_allocation_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY fleet_allocation_events_isolation ON fleet_allocation_events
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON fleet_allocation_events TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE fleet_allocation_events_id_seq TO ggscale_app;

-- Cap per-allocation history at 50 rows. The trigger runs after each insert
-- and deletes the oldest events that fall past the cap. Cheap because the
-- (allocation_id, id DESC) index serves the LIMIT/OFFSET scan.
CREATE OR REPLACE FUNCTION fleet_allocation_events_trim() RETURNS TRIGGER AS $$
BEGIN
    DELETE FROM fleet_allocation_events
    WHERE allocation_id = NEW.allocation_id
      AND id IN (
          SELECT id FROM fleet_allocation_events
          WHERE allocation_id = NEW.allocation_id
          ORDER BY id DESC
          OFFSET 50
      );
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER fleet_allocation_events_trim_trg
    AFTER INSERT ON fleet_allocation_events
    FOR EACH ROW EXECUTE FUNCTION fleet_allocation_events_trim();
