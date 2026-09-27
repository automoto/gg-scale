-- Operators must pause enqueue and stop workers before this migration.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM matchmaking_tickets WHERE claim_id IS NOT NULL) THEN
  RAISE EXCEPTION 'settle matchmaking claims before party cutover';
 END IF;
END $$;
INSERT INTO matchmaking_entries (id,tenant_id,project_id,status)
SELECT id,tenant_id,project_id,status FROM matchmaking_tickets WHERE entry_id IS NULL;
UPDATE matchmaking_tickets SET entry_id=id WHERE entry_id IS NULL;
SELECT setval('matchmaking_entries_id_seq', GREATEST(1,COALESCE((SELECT max(id) FROM matchmaking_entries),0)), EXISTS(SELECT 1 FROM matchmaking_entries));
ALTER TABLE matchmaking_tickets ALTER COLUMN entry_id SET NOT NULL;
CREATE INDEX matchmaking_tickets_entry ON matchmaking_tickets(entry_id);

-- Existing solo insertion remains compatible, but every ticket gets an entry.
CREATE FUNCTION matchmaking_solo_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.entry_id IS NULL THEN
  PERFORM pg_advisory_xact_lock(hashtextextended('party_member:' || NEW.project_id::text, NEW.player_id));
  IF EXISTS (SELECT 1 FROM party_members WHERE project_id=NEW.project_id AND player_id=NEW.player_id) THEN
   RAISE EXCEPTION 'party_member_must_leave' USING ERRCODE='P0001';
  END IF;
  INSERT INTO matchmaking_entries(tenant_id,project_id) VALUES(NEW.tenant_id,NEW.project_id) RETURNING id INTO NEW.entry_id;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER matchmaking_solo_entry BEFORE INSERT ON matchmaking_tickets FOR EACH ROW EXECUTE FUNCTION matchmaking_solo_entry();
