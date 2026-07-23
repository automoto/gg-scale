-- Machine-readable reason a matchmaking ticket ended in 'failed', surfaced in
-- the ticket poll response so clients can tell a timeout ('expired') from a
-- resolver that never succeeded ('attempts_exhausted'). Nullable: only failed
-- tickets carry a reason. The CHECK bounds current values; new reasons are
-- added by forward migration (the API documents it as an open enum).
ALTER TABLE matchmaking_tickets
    ADD COLUMN failure_reason text
        CHECK (failure_reason IN ('expired', 'attempts_exhausted'));
