-- Friend codes: a short shareable identifier every player gets (generated
-- lazily on first profile read, so no backfill is needed). Resolving a code
-- yields the player's public profile; the existing friend-request route does
-- the rest. Unique per project so a code shared out of band is unambiguous.
ALTER TABLE project_players
ADD COLUMN friend_code text;

CREATE UNIQUE INDEX project_players_friend_code_uniq
ON project_players (tenant_id, project_id, friend_code)
WHERE friend_code IS NOT NULL AND deleted_at IS NULL;
