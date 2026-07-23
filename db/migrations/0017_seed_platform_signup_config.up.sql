-- Heal the platform_signup_config singleton. Migration 0004 seeds this row, but
-- environments reset from a baseline dump that predated the seed can be missing
-- it, which makes GetPublicSignupEnabled (a :one query on WHERE id = 1) return
-- no rows and 500 the admin tenant-signups page. Re-assert the row idempotently;
-- ON CONFLICT DO NOTHING preserves any already-configured value.
INSERT INTO platform_signup_config (id, public_tenant_signup_enabled)
VALUES (1, false)
ON CONFLICT (id) DO NOTHING;
