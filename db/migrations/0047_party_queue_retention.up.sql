CREATE INDEX matchmaking_entries_bucket ON matchmaking_entries(tenant_id,project_id,status,created_at,id);
CREATE INDEX matchmaking_entries_retention ON matchmaking_entries(created_at) WHERE status<>'queued';
CREATE INDEX parties_retention ON parties(closed_at) WHERE state='closed';
CREATE INDEX parties_current_entry ON parties(current_queue_entry_id) WHERE current_queue_entry_id IS NOT NULL;
CREATE INDEX matchmaking_entries_party ON matchmaking_entries(party_id) WHERE party_id IS NOT NULL;

ALTER TABLE party_invites DROP CONSTRAINT party_invites_tenant_id_project_id_party_id_fkey;
ALTER TABLE party_invites ADD FOREIGN KEY(tenant_id,project_id,party_id)
 REFERENCES parties(tenant_id,project_id,id) ON DELETE CASCADE;
ALTER TABLE party_invite_codes DROP CONSTRAINT party_invite_codes_tenant_id_project_id_party_id_fkey;
ALTER TABLE party_invite_codes ADD FOREIGN KEY(tenant_id,project_id,party_id)
 REFERENCES parties(tenant_id,project_id,id) ON DELETE CASCADE;
CREATE INDEX party_invite_codes_party ON party_invite_codes(party_id);
CREATE INDEX party_invites_party ON party_invites(party_id);
