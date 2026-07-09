-- Refresh-token reuse detection. revoked_reason records WHY a session row was
-- revoked so /v1/auth/refresh can tell a replayed *rotated* token (a theft
-- signal — the legit client already rotated past it, so the whole session set
-- is nuked) from a benign logged-out one (a stale client retry, left alone).
-- NULL on historical rows is treated as "unknown / not rotated": fail closed
-- to a plain 401 without the family kill.
ALTER TABLE public.sessions ADD COLUMN revoked_reason text;
