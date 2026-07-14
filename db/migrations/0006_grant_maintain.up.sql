-- River's reindexer runs daily as ggscale_app, but REINDEX (PG 17: the
-- MAINTAIN privilege) was never granted, so every run fails with a permission
-- error on the postgres-owned river_* tables. Grant MAINTAIN schema-wide —
-- it also covers VACUUM/ANALYZE and any future maintenance jobs — and set
-- default privileges so tables created by later migrations inherit it.
GRANT MAINTAIN ON ALL TABLES IN SCHEMA public TO ggscale_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT MAINTAIN ON TABLES TO ggscale_app;
