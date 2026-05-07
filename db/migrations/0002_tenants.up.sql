-- Tenants are the billing/isolation root. Projects partition a tenant's
-- workloads (a tenant might run several games). API keys authenticate
-- machine-to-machine callers and are always scoped to a tenant; an api_key
-- may also pin to a single project.

CREATE TABLE tenants (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    tier        TEXT NOT NULL DEFAULT 'free' CHECK (tier IN ('free', 'payg', 'premium')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX tenants_name_idx ON tenants (name) WHERE deleted_at IS NULL;

CREATE TABLE projects (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);

CREATE INDEX projects_tenant_id_idx ON projects (tenant_id);
CREATE UNIQUE INDEX projects_tenant_name_uniq ON projects (tenant_id, name) WHERE deleted_at IS NULL;

CREATE TABLE api_keys (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    project_id  BIGINT REFERENCES projects(id) ON DELETE CASCADE,
    key_hash    BYTEA NOT NULL UNIQUE,
    label       TEXT,
    scopes      TEXT[] NOT NULL DEFAULT '{}',
    -- key_type splits Stripe-style publishable (embedded in shipped game
    -- binaries) from secret (game-server / tenant-backend only). Sensitive
    -- writes (fleet register, leaderboard submit) require 'secret'.
    key_type    TEXT NOT NULL DEFAULT 'secret'
                CHECK (key_type IN ('publishable', 'secret')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX api_keys_tenant_id_idx ON api_keys (tenant_id);
CREATE INDEX api_keys_project_id_idx ON api_keys (project_id) WHERE project_id IS NOT NULL;
CREATE INDEX api_keys_active_idx ON api_keys (tenant_id) WHERE revoked_at IS NULL;
CREATE INDEX api_keys_type_idx ON api_keys (tenant_id, key_type) WHERE revoked_at IS NULL;
