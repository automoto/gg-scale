DROP FUNCTION IF EXISTS public.control_panel_create_tenant_bare(bigint, text);
DROP INDEX IF EXISTS tenants_name_unique_idx;
DROP TABLE IF EXISTS platform_signup_config;
DROP TABLE IF EXISTS tenant_signup_requests;
