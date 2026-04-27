-- Usage samples drive billing. Range-partitioned monthly so old months can
-- be archived/dropped cheaply. The DO block creates 12 forward partitions;
-- a maintenance job (Phase 4) extends the window every month.

CREATE TABLE usage_samples (
    id              BIGSERIAL,
    tenant_id       BIGINT NOT NULL,
    project_id      BIGINT NOT NULL,
    ts              TIMESTAMPTZ NOT NULL,
    ccu             INT NOT NULL DEFAULT 0,
    requests        BIGINT NOT NULL DEFAULT 0,
    bytes_egress    BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);

CREATE INDEX usage_samples_tenant_ts_idx ON usage_samples (tenant_id, ts);
CREATE INDEX usage_samples_project_ts_idx ON usage_samples (project_id, ts);

DO $$
DECLARE
    base_month  DATE := date_trunc('month', now())::date;
    part_start  DATE;
    part_end    DATE;
    part_name   TEXT;
    i           INT;
BEGIN
    FOR i IN 0..11 LOOP
        part_start := base_month + (i || ' months')::interval;
        part_end   := base_month + ((i + 1) || ' months')::interval;
        part_name  := 'usage_samples_' || to_char(part_start, 'YYYY_MM');
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF usage_samples FOR VALUES FROM (%L) TO (%L)',
            part_name, part_start, part_end
        );
    END LOOP;
END$$;
