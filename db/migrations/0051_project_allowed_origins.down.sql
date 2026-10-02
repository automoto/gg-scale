DROP FUNCTION public.all_project_allowed_origins();
ALTER TABLE projects DROP COLUMN allowed_origins;
