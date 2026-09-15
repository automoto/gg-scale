DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM matchmaking_entries WHERE party_id IS NOT NULL AND status='queued')
 OR EXISTS(SELECT 1 FROM matchmaking_tickets WHERE claim_id IS NOT NULL) THEN
  RAISE EXCEPTION 'settle claims and party entries before reverting the cutover';
 END IF;
END $$;
DROP TRIGGER matchmaking_solo_entry ON matchmaking_tickets;
DROP FUNCTION matchmaking_solo_entry();
ALTER TABLE matchmaking_tickets ALTER COLUMN entry_id DROP NOT NULL;
DROP INDEX matchmaking_tickets_entry;
