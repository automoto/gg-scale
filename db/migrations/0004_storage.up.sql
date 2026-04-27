-- Per-end-user JSON KV store. Optimistic concurrency via the version column;
-- callers may pass If-Match: <version> on PUT.

CREATE TABLE storage_objects (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id      BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    owner_user_id   BIGINT NOT NULL REFERENCES end_users(id) ON DELETE CASCADE,
    key             TEXT NOT NULL,
    value           JSONB NOT NULL,
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

CREATE INDEX storage_objects_tenant_id_idx ON storage_objects (tenant_id);
CREATE UNIQUE INDEX storage_objects_owner_key_uniq
    ON storage_objects (tenant_id, project_id, owner_user_id, key)
    WHERE deleted_at IS NULL;
