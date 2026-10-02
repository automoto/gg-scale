-- The base row holds the value from before revision history, which no
-- tracked actor wrote. It used to copy the source of the first tracked write,
-- so a base row before an MCP write showed source 'mcp'.
ALTER TABLE settings_revisions DROP CONSTRAINT settings_revisions_source_check;
ALTER TABLE settings_revisions ADD CONSTRAINT settings_revisions_source_check
    CHECK (source IN ('dashboard', 'mcp', 'rollback', 'base'));

-- A base row is revision 1 with no actor, written in the same transaction as
-- revision 2, so the two rows share created_at (now() is fixed per
-- transaction). A create by a user who was deleted later also has no actor,
-- but its revision 2 comes from a later transaction.
UPDATE settings_revisions b
SET source = 'base'
WHERE b.revision = 1
  AND b.actor_user_id IS NULL
  AND b.mcp_token_id IS NULL
  AND EXISTS (
      SELECT 1 FROM settings_revisions n
      WHERE n.tenant_id = b.tenant_id
        AND n.resource_kind = b.resource_kind
        AND n.resource_id = b.resource_id
        AND n.revision = 2
        AND n.created_at = b.created_at
  );
