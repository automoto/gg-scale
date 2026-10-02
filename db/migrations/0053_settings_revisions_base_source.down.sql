UPDATE settings_revisions SET source = 'dashboard' WHERE source = 'base';
ALTER TABLE settings_revisions DROP CONSTRAINT settings_revisions_source_check;
ALTER TABLE settings_revisions ADD CONSTRAINT settings_revisions_source_check
    CHECK (source IN ('dashboard', 'mcp', 'rollback'));
