-- MCP tokens become management tokens: the same tokens now also work on the
-- /v1/project HTTP API. This is a direct rename, so the previous release
-- stops working once it runs. The owner approved this exception to the
-- expand/contract rule (docs/management-api-plan.md, "Database rename").
ALTER TABLE mcp_tokens RENAME TO management_tokens;
ALTER SEQUENCE mcp_tokens_id_seq RENAME TO management_tokens_id_seq;
ALTER INDEX mcp_tokens_pkey RENAME TO management_tokens_pkey;
ALTER INDEX mcp_tokens_hash_uniq RENAME TO management_tokens_hash_uniq;
ALTER INDEX mcp_tokens_project_idx RENAME TO management_tokens_project_idx;
ALTER TABLE management_tokens RENAME CONSTRAINT mcp_tokens_tenant_id_fkey TO management_tokens_tenant_id_fkey;
ALTER TABLE management_tokens RENAME CONSTRAINT mcp_tokens_project_id_fkey TO management_tokens_project_id_fkey;
ALTER TABLE management_tokens RENAME CONSTRAINT mcp_tokens_created_by_user_id_fkey TO management_tokens_created_by_user_id_fkey;
ALTER TABLE management_tokens RENAME CONSTRAINT mcp_tokens_label_check TO management_tokens_label_check;
ALTER POLICY mcp_tokens_bootstrap ON management_tokens RENAME TO management_tokens_bootstrap;
ALTER POLICY mcp_tokens_isolation ON management_tokens RENAME TO management_tokens_isolation;

ALTER TABLE settings_revisions RENAME COLUMN mcp_token_id TO management_token_id;
ALTER TABLE settings_revisions RENAME CONSTRAINT settings_revisions_mcp_token_fk TO settings_revisions_management_token_fk;

ALTER TABLE settings_revisions DROP CONSTRAINT settings_revisions_source_check;
UPDATE settings_revisions SET source = 'token' WHERE source = 'mcp';
ALTER TABLE settings_revisions ADD CONSTRAINT settings_revisions_source_check
    CHECK (source IN ('dashboard', 'token', 'rollback', 'base'));
