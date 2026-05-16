DROP INDEX IF EXISTS game_server_allocations_fleet_id_idx;
ALTER TABLE game_server_allocations DROP COLUMN IF EXISTS fleet_id;
