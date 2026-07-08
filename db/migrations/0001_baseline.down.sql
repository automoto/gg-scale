-- Teardown for the squashed baseline: drop every object the baseline created.
-- golang-migrate's own schema_migrations table lives in the public schema, so
-- a blanket DROP SCHEMA would take it with it and break the migrator — drop the
-- app's tables, routines, and extensions explicitly and leave it alone. The
-- cluster-level ggscale_app role is created idempotently by the up migration
-- and is intentionally left in place.
DO $$
DECLARE
    obj record;
BEGIN
    -- Tables: dropping the non-partition tables CASCADE takes their partitions,
    -- owned sequences, indexes, RLS policies, and dependent views with them.
    FOR obj IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind IN ('r', 'p')
          AND c.relispartition = false
          AND c.relname <> 'schema_migrations'
    LOOP
        EXECUTE format('DROP TABLE IF EXISTS public.%I CASCADE', obj.relname);
    END LOOP;

    -- Functions and procedures, skipping those owned by an extension (they are
    -- removed when the extension is dropped below).
    FOR obj IN
        SELECT format('public.%I(%s)', p.proname,
                      pg_get_function_identity_arguments(p.oid)) AS sig
        FROM pg_proc p
        JOIN pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = 'public'
          AND NOT EXISTS (
              SELECT 1 FROM pg_depend d
              WHERE d.objid = p.oid AND d.deptype = 'e'
          )
    LOOP
        EXECUTE format('DROP ROUTINE IF EXISTS %s CASCADE', obj.sig);
    END LOOP;
END$$;

DROP EXTENSION IF EXISTS pg_trgm;
DROP EXTENSION IF EXISTS citext;
DROP EXTENSION IF EXISTS pgcrypto;
