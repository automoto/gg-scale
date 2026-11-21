ALTER TABLE settings_revisions DROP CONSTRAINT settings_revisions_source_check;
UPDATE settings_revisions SET source = 'mcp' WHERE source = 'token';
ALTER TABLE settings_revisions ADD CONSTRAINT settings_revisions_source_check
    CHECK (source IN ('dashboard', 'mcp', 'rollback', 'base'));

ALTER TABLE settings_revisions RENAME CONSTRAINT settings_revisions_management_token_fk TO settings_revisions_mcp_token_fk;
ALTER TABLE settings_revisions RENAME COLUMN management_token_id TO mcp_token_id;

ALTER POLICY management_tokens_isolation ON management_tokens RENAME TO mcp_tokens_isolation;
ALTER POLICY management_tokens_bootstrap ON management_tokens RENAME TO mcp_tokens_bootstrap;
ALTER TABLE management_tokens RENAME CONSTRAINT management_tokens_label_check TO mcp_tokens_label_check;
ALTER TABLE management_tokens RENAME CONSTRAINT management_tokens_created_by_user_id_fkey TO mcp_tokens_created_by_user_id_fkey;
ALTER TABLE management_tokens RENAME CONSTRAINT management_tokens_project_id_fkey TO mcp_tokens_project_id_fkey;
ALTER TABLE management_tokens RENAME CONSTRAINT management_tokens_tenant_id_fkey TO mcp_tokens_tenant_id_fkey;
ALTER INDEX management_tokens_project_idx RENAME TO mcp_tokens_project_idx;
ALTER INDEX management_tokens_hash_uniq RENAME TO mcp_tokens_hash_uniq;
ALTER INDEX management_tokens_pkey RENAME TO mcp_tokens_pkey;
ALTER SEQUENCE management_tokens_id_seq RENAME TO mcp_tokens_id_seq;
ALTER TABLE management_tokens RENAME TO mcp_tokens;
