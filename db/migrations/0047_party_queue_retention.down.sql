DROP INDEX party_invites_party;
DROP INDEX party_invite_codes_party;
ALTER TABLE party_invite_codes DROP CONSTRAINT party_invite_codes_tenant_id_project_id_party_id_fkey;
ALTER TABLE party_invite_codes ADD FOREIGN KEY(tenant_id,project_id,party_id)
 REFERENCES parties(tenant_id,project_id,id);
ALTER TABLE party_invites DROP CONSTRAINT party_invites_tenant_id_project_id_party_id_fkey;
ALTER TABLE party_invites ADD FOREIGN KEY(tenant_id,project_id,party_id)
 REFERENCES parties(tenant_id,project_id,id);
DROP INDEX matchmaking_entries_party;
DROP INDEX parties_current_entry;
DROP INDEX parties_retention;
DROP INDEX matchmaking_entries_retention;
DROP INDEX matchmaking_entries_bucket;
