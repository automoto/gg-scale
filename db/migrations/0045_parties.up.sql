CREATE TABLE parties (
 id bigserial PRIMARY KEY,
 tenant_id bigint NOT NULL REFERENCES tenants(id),
 project_id bigint NOT NULL REFERENCES projects(id),
 leader_id bigint REFERENCES project_players(id) ON DELETE SET NULL,
 state text NOT NULL DEFAULT 'idle' CHECK (state IN ('idle','queued','matched','closed')),
 version bigint NOT NULL DEFAULT 1,
 roster_version bigint NOT NULL DEFAULT 1,
 settings jsonb NOT NULL DEFAULT '{}',
 max_members integer NOT NULL DEFAULT 8 CHECK (max_members BETWEEN 1 AND 8),
 current_queue_entry_id bigint,
 last_match_id text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 closed_at timestamptz,
 UNIQUE (tenant_id,project_id,id)
);
CREATE TABLE party_members (
 tenant_id bigint NOT NULL,
 project_id bigint NOT NULL,
 party_id bigint NOT NULL,
 player_id bigint NOT NULL REFERENCES project_players(id),
 ready_version bigint NOT NULL DEFAULT 0,
 string_properties jsonb NOT NULL DEFAULT '{}',
 numeric_properties jsonb NOT NULL DEFAULT '{}',
 attributes jsonb NOT NULL DEFAULT '{}',
 joined_at timestamptz NOT NULL DEFAULT now(),
 last_seen_at timestamptz NOT NULL DEFAULT now(),
 disconnect_deadline timestamptz NOT NULL DEFAULT now() + interval '30 seconds',
 PRIMARY KEY (party_id,player_id),
 UNIQUE (tenant_id,project_id,player_id),
 FOREIGN KEY (tenant_id,project_id,party_id) REFERENCES parties(tenant_id,project_id,id)
);
CREATE INDEX party_members_disconnect ON party_members(disconnect_deadline);
CREATE TABLE party_invites (
 id bigserial PRIMARY KEY,
 tenant_id bigint NOT NULL,
 project_id bigint NOT NULL,
 party_id bigint NOT NULL,
 target_id bigint NOT NULL REFERENCES project_players(id) ON DELETE CASCADE,
 expires_at timestamptz NOT NULL DEFAULT now() + interval '5 minutes',
 status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','accepted','declined','revoked','expired')),
 FOREIGN KEY (tenant_id,project_id,party_id) REFERENCES parties(tenant_id,project_id,id)
);
CREATE UNIQUE INDEX party_pending_invite ON party_invites(party_id,target_id) WHERE status = 'pending';
CREATE TABLE party_invite_codes (
 id bigserial PRIMARY KEY,
 tenant_id bigint NOT NULL,
 project_id bigint NOT NULL,
 party_id bigint NOT NULL,
 code_hash bytea NOT NULL UNIQUE CHECK (octet_length(code_hash)=32),
 max_uses integer NOT NULL DEFAULT 7 CHECK (max_uses BETWEEN 1 AND 7),
 uses integer NOT NULL DEFAULT 0,
 expires_at timestamptz NOT NULL DEFAULT now() + interval '5 minutes',
 revoked_at timestamptz,
 FOREIGN KEY (tenant_id,project_id,party_id) REFERENCES parties(tenant_id,project_id,id)
);
CREATE TABLE party_code_attempts (
 tenant_id bigint NOT NULL,
 project_id bigint NOT NULL,
 subject text NOT NULL,
 failures integer NOT NULL DEFAULT 0,
 window_start timestamptz NOT NULL DEFAULT now(),
 blocked_until timestamptz,
 PRIMARY KEY (tenant_id,project_id,subject)
);
CREATE TABLE matchmaking_entries (
 id bigserial PRIMARY KEY,
 tenant_id bigint NOT NULL,
 project_id bigint NOT NULL,
 party_id bigint,
 status ticket_status NOT NULL DEFAULT 'queued',
 idempotency_key text,
 previous_match_id text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (party_id,idempotency_key),
 UNIQUE (tenant_id,project_id,id),
 FOREIGN KEY (tenant_id,project_id,party_id) REFERENCES parties(tenant_id,project_id,id)
);
CREATE UNIQUE INDEX matchmaking_party_active ON matchmaking_entries(party_id) WHERE status='queued';
ALTER TABLE matchmaking_tickets ADD COLUMN entry_id bigint REFERENCES matchmaking_entries(id), ADD COLUMN party_id bigint REFERENCES parties(id);

DO $$ DECLARE tab text; BEGIN
 FOREACH tab IN ARRAY ARRAY['parties','party_members','party_invites','party_invite_codes','party_code_attempts','matchmaking_entries'] LOOP
  EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY',tab);
  EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY',tab);
  EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant_id = NULLIF(current_setting(''app.tenant_id'',true),'''')::bigint) WITH CHECK (tenant_id = NULLIF(current_setting(''app.tenant_id'',true),'''')::bigint)',tab);
  EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON %I TO ggscale_app',tab);
 END LOOP;
 FOREACH tab IN ARRAY ARRAY['parties','party_members','matchmaking_entries'] LOOP
  EXECUTE format('CREATE POLICY worker_access ON %I USING (NULLIF(current_setting(''app.tenant_id'',true),'''') IS NULL) WITH CHECK (NULLIF(current_setting(''app.tenant_id'',true),'''') IS NULL)',tab);
 END LOOP;
END $$;
GRANT USAGE,SELECT ON SEQUENCE parties_id_seq,party_invites_id_seq,party_invite_codes_id_seq,matchmaking_entries_id_seq TO ggscale_app;

-- Scope is enforced even for direct SQL writers, in addition to tenant RLS.
ALTER TABLE projects ADD CONSTRAINT party_project_scope UNIQUE(tenant_id,id);
ALTER TABLE project_players ADD CONSTRAINT party_player_scope UNIQUE(tenant_id,project_id,id);
ALTER TABLE parties ADD FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id);
ALTER TABLE parties ADD FOREIGN KEY(tenant_id,project_id,leader_id) REFERENCES project_players(tenant_id,project_id,id);
ALTER TABLE party_members ADD FOREIGN KEY(tenant_id,project_id,player_id) REFERENCES project_players(tenant_id,project_id,id);
ALTER TABLE party_invites ADD FOREIGN KEY(tenant_id,project_id,target_id) REFERENCES project_players(tenant_id,project_id,id);
ALTER TABLE matchmaking_entries ADD FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id);
ALTER TABLE matchmaking_tickets ADD FOREIGN KEY(tenant_id,project_id,entry_id) REFERENCES matchmaking_entries(tenant_id,project_id,id);

CREATE FUNCTION party_live_leader() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE party_key bigint;
BEGIN
 IF TG_TABLE_NAME='parties' THEN party_key=COALESCE(NEW.id,OLD.id);
 ELSE party_key=COALESCE(NEW.party_id,OLD.party_id); END IF;
 IF EXISTS(SELECT 1 FROM parties p WHERE p.id=party_key AND p.state<>'closed'
  AND NOT EXISTS(SELECT 1 FROM party_members m WHERE m.party_id=p.id AND m.player_id=p.leader_id)) THEN
  RAISE EXCEPTION 'live party leader must be a member' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER party_live_leader AFTER INSERT OR UPDATE ON parties DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION party_live_leader();
CREATE CONSTRAINT TRIGGER party_member_leader AFTER INSERT OR UPDATE OR DELETE ON party_members DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION party_live_leader();

-- The IP budget is global. Keep it separate from tenant-scoped player budgets.
-- Only this narrow function can access the table as the application role.
CREATE TABLE party_code_ip_attempts (
 ip text PRIMARY KEY,
 failures integer NOT NULL DEFAULT 0,
 window_start timestamptz NOT NULL DEFAULT now(),
 blocked_until timestamptz
);
CREATE FUNCTION party_code_ip_limit(source_ip text, failed boolean) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=public,pg_temp AS $$
DECLARE blocked boolean;
BEGIN
 INSERT INTO party_code_ip_attempts(ip) VALUES(source_ip) ON CONFLICT DO NOTHING;
 SELECT COALESCE(blocked_until>now(),false) INTO blocked FROM party_code_ip_attempts WHERE ip=source_ip FOR UPDATE;
 IF blocked THEN RETURN true; END IF;
 IF failed THEN
  UPDATE party_code_ip_attempts SET
   failures=CASE WHEN window_start<=now()-interval '15 minutes' THEN 1 ELSE failures+1 END,
   window_start=CASE WHEN window_start<=now()-interval '15 minutes' THEN now() ELSE window_start END,
   blocked_until=CASE WHEN window_start>now()-interval '15 minutes' AND failures+1>=100 THEN now()+interval '15 minutes' ELSE NULL END
  WHERE ip=source_ip;
 END IF;
 RETURN false;
END $$;
REVOKE ALL ON FUNCTION party_code_ip_limit(text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION party_code_ip_limit(text,boolean) TO ggscale_app;

CREATE TABLE matchmaking_resolutions (
 id text PRIMARY KEY,
 tenant_id bigint NOT NULL,
 project_id bigint NOT NULL,
 expires_at timestamptz NOT NULL,
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(tenant_id,project_id) REFERENCES projects(tenant_id,id)
);
ALTER TABLE matchmaking_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE matchmaking_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON matchmaking_resolutions USING(tenant_id=NULLIF(current_setting('app.tenant_id',true),'')::bigint);
CREATE POLICY worker_access ON matchmaking_resolutions USING(NULLIF(current_setting('app.tenant_id',true),'') IS NULL);
GRANT SELECT,INSERT,UPDATE,DELETE ON matchmaking_resolutions TO ggscale_app;
CREATE INDEX matchmaking_resolutions_expiry ON matchmaking_resolutions(expires_at);
CREATE INDEX party_latest_ticket ON matchmaking_tickets(party_id,player_id,created_at DESC,id DESC) WHERE party_id IS NOT NULL;
CREATE INDEX allocation_resolution ON game_server_allocations((metadata->>'ggscale.dev/resolution-id'));

ALTER TABLE parties ADD FOREIGN KEY(tenant_id,project_id,current_queue_entry_id) REFERENCES matchmaking_entries(tenant_id,project_id,id);
