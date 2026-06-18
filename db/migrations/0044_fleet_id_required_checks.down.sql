ALTER TABLE matchmaking_tickets
    DROP CONSTRAINT IF EXISTS matchmaking_tickets_fleet_id_required;

ALTER TABLE game_server_allocations
    DROP CONSTRAINT IF EXISTS game_server_allocations_fleet_id_required;
