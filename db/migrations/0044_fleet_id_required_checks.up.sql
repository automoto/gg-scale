ALTER TABLE game_server_allocations
    ADD CONSTRAINT game_server_allocations_fleet_id_required
    CHECK (fleet_id IS NOT NULL)
    NOT VALID;

ALTER TABLE matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_fleet_id_required
    CHECK (fleet_id IS NOT NULL)
    NOT VALID;
