-- Fleets are operator-defined templates that an allocation is drawn from.
-- A fleet captures the backend-specific recipe (Docker image+port+probe,
-- Agones fleet name+selector labels, or opaque plugin config) under a
-- project-scoped name. Allocations reference a fleet by id; matchmaker
-- tickets reference a fleet by id; the client-facing API and SDK identify
-- a fleet by its project-scoped name.
--
-- The single `config` JSONB column carries per-backend fields so the schema
-- doesn't fork by backend. Validation lives in application code where it can
-- give precise error messages per backend type.
--
-- Soft delete (`deleted_at`) so historical allocations can still display
-- "this came from fleet X" without an FK violation when an operator retires
-- a template.

CREATE TABLE fleets (
    id           BIGSERIAL PRIMARY KEY,
    tenant_id    BIGINT NOT NULL REFERENCES tenants(id)  ON DELETE CASCADE,
    project_id   BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT   NOT NULL,
    backend      TEXT   NOT NULL,
    config       JSONB  NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ
);

-- Name is unique per project, ignoring soft-deleted rows so an operator can
-- recreate a fleet under the same name after retiring the old one.
CREATE UNIQUE INDEX fleets_project_name_active_uidx
    ON fleets (project_id, name)
    WHERE deleted_at IS NULL;

CREATE INDEX fleets_project_active_idx
    ON fleets (project_id)
    WHERE deleted_at IS NULL;

CREATE INDEX fleets_tenant_id_idx
    ON fleets (tenant_id);

ALTER TABLE fleets ENABLE ROW LEVEL SECURITY;
CREATE POLICY fleets_isolation ON fleets
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true)::bigint);

GRANT SELECT, INSERT, UPDATE, DELETE ON fleets TO ggscale_app;
GRANT USAGE, SELECT ON SEQUENCE fleets_id_seq TO ggscale_app;
