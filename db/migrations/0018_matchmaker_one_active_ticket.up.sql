-- One active matchmaking ticket per player per project, step 1 of 2: collapse
-- any pre-existing duplicates so the unique index can build. Keep the newest
-- queued ticket per (tenant, project, player) and cancel the rest. The index
-- itself is built CONCURRENTLY in the next migration — CREATE INDEX
-- CONCURRENTLY cannot share a transaction with other statements, so it has to
-- live in its own single-statement file.
WITH ranked AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY tenant_id, project_id, player_id
               ORDER BY created_at DESC, id DESC
           ) AS rn
    FROM matchmaking_tickets
    WHERE status = 'queued'
)
UPDATE matchmaking_tickets t
SET status           = 'cancelled',
    claim_id         = NULL,
    claimed_at       = NULL,
    claim_expires_at = NULL
FROM ranked r
WHERE t.id = r.id
  AND r.rn > 1;
