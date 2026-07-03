DROP FUNCTION IF EXISTS project_join_context(BIGINT);
ALTER TABLE projects DROP COLUMN IF EXISTS public_joining_enabled;
ALTER TABLE tenants DROP COLUMN IF EXISTS public_joining_enabled;
