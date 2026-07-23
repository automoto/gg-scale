-- The host player of a match: for match_only and game_session results the
-- longest-waiting player (the group's oldest ticket) hosts, and peers connect
-- to them (P2P / listen-server). NULL for fleet_allocation, where a dedicated
-- server — not a player — is the endpoint. Surfaced in the matched event and
-- the ticket poll response. The roster (persisted JSON) already carries each
-- member's opaque attributes; no column is needed for those.
ALTER TABLE matchmaker_matches
    ADD COLUMN host_player_id bigint;
