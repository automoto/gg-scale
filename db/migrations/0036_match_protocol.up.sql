-- Add match_protocol so the matchmaker can return a protocol_hint
-- alongside match_address. The agones backend populates it by reading
-- the allocated GameServer's Spec.Ports[0].Protocol; other backends
-- leave it empty. Clients use it for defense-in-depth (cross-game
-- launchers, dashboards) — the game's own client is built for a
-- specific transport and trusts that as ground truth.

ALTER TABLE matchmaking_tickets
    ADD COLUMN match_protocol TEXT NOT NULL DEFAULT '';
