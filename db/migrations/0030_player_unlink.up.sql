-- Player-initiated non-destructive unlink: the row and its game data stay,
-- the account link goes inactive, and player-credential auth (email login,
-- refresh) is blocked until a re-invite clears the marker.
ALTER TABLE project_players
    ADD COLUMN unlinked_at timestamptz;
