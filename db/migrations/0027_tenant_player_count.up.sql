-- tenants.player_count: registered (non-soft-deleted) players per tenant,
-- maintained explicitly by the Go create paths (no triggers). It replaces the
-- SELECT ... FOR UPDATE + COUNT(*) quota gate that serialized every player
-- create per tenant. Invariants:
--   * every project_players INSERT path reserves a slot via
--     ReserveTenantPlayerSlot in the same tx — the conditional increment
--     enforces the class player cap atomically for enforced tenants and
--     still counts unenforced ones, so the counter is exact for every
--     tenant and enforce_quotas can be flipped on at any time;
--   * out-of-band player writes (bulk seeds, imports, manual SQL) must
--     maintain the counter themselves, as this backfill does;
--   * players are never deleted today; any future delete/restore path must
--     decrement/increment it in the same transaction.
--
-- row_security = off: the backfill must see every tenant's players. The
-- migration role either may bypass RLS (and the backfill is exact) or the
-- migration fails loudly — it can never silently backfill zeros.
SET LOCAL row_security = off;

ALTER TABLE tenants ADD COLUMN player_count bigint NOT NULL DEFAULT 0;

UPDATE tenants t
SET player_count = (
    SELECT count(*)
    FROM project_players p
    WHERE p.tenant_id = t.id
      AND p.deleted_at IS NULL
);
