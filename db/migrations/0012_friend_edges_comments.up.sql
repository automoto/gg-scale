-- Documents the upsert contract for the friend-request handler. The unique
-- index enforces "one current row per directed (from, to) pair"; the
-- handler in POST /v1/friends/:id/request must UPSERT — INSERT ... ON
-- CONFLICT (tenant_id, from_user_id, to_user_id) DO UPDATE — so a
-- rejected edge can be re-requested, while accepted/pending stay
-- idempotent and blocked is terminal.

COMMENT ON TABLE friend_edges IS
    'One directed edge per (tenant, from, to). Status transitions are '
    'managed via UPSERT in the handler: a re-request after rejection '
    'updates status pending; pending/accepted are idempotent; blocked '
    'is terminal. See migration 0012 for the contract.';

COMMENT ON INDEX friend_edges_pair_uniq IS
    'One current row per directed (from, to) pair. Re-requests after a '
    'rejection update the existing row in place via ON CONFLICT DO UPDATE.';
